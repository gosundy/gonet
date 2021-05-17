package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/felixge/fgprof"

	"github.com/gorilla/websocket"
	"github.com/gosundy/streambuffer"
	"github.com/hslam/mux"
	"github.com/hslam/response"
	"github.com/panjf2000/ants/v2"
	"golang.org/x/sys/unix"
)

type Listener struct {
	fd   int
	addr net.Addr
}
type Conn struct {
	fd             int
	buffer         []byte
	inboundBuffer  *buffer.Buffer
	outboundBuffer *buffer.Buffer
	context        interface{}
	workerBind     bool
	poller         *Poller
	readCond       *sync.Cond
	opened         bool
}

var count = int64(0)

type Worker struct {
	conn *Conn
}
type Response struct {
	status     string // e.g. "200 OK"
	statusCode int    // e.g. 200
	buffer     *ResponseBuffer
	len        int
}
type ResponseBuffer struct {
	*bytes.Buffer
	io.Closer
}

type HandlerFunc interface {
	UpgradeFunc(conn *Conn) (context interface{}, err error)
	ServeFunc(context interface{}) error
}

type Robbin struct {
	curIdx  int
	total   int
	pollers []*Poller
	mux     sync.Mutex
}
type Server struct {
	network     string
	addr        string
	numListener int
	handlerFunc HandlerFunc
}

//http handler
type HttpHandler struct {
}
type HttpContext struct {
	reader *bufio.Reader
	rw     *bufio.ReadWriter
	conn   *Conn
}

//tcp handler
type TcpHandler struct {
}
type TcpContext struct {
	reader *bufio.Reader
	conn   *Conn
}

// websocket
type WebsocketHandler struct {
}
type WebsocketContext struct {
	reader *bufio.Reader
	rw     *bufio.ReadWriter
	conn   net.Conn
}
type FdToConns struct {
	total     int
	fdToConns []*FdToConn
}
type FdToConn struct {
	fdToConn map[int]*Conn
	mux      *sync.RWMutex
}

var pool = new(sync.Pool)
var connPool = new(sync.Pool)
var fdToConns *FdToConns
var robbinController *Robbin
var gPool *ants.Pool
var handlerFunc HandlerFunc
var m = mux.New()
var bytesPool = new(sync.Pool)

func main() {
	flagNetwork := ""
	flagAddr := ""
	flagNumListener := 0
	flagNumGo := 0
	flagSubWorker := 0
	flag.StringVar(&flagNetwork, "network", "tcp", "tcp")
	flag.StringVar(&flagAddr, "addr", "0.0.0.0:8080", "addr")
	flag.IntVar(&flagNumListener, "listener", 1, "count of listener")
	flag.IntVar(&flagNumGo, "go", 1000, "count of go")
	flag.IntVar(&flagSubWorker, "sub", 1, "count of go")
	flag.Parse()

	server := NewServer(flagNetwork, flagAddr, flagNumListener)
	err := server.Serve()
	if err != nil {
		panic(err)
	}
	fdToConns = NewFdToConns(5)
	bytesPool.New = func() interface{} {
		return make([]byte, 4096)
	}
	connPool.New = func() interface{} {
		return &Conn{}
	}
	handlerFunc = &WebsocketHandler{}
	gPool, err = ants.NewPool(flagNumGo, ants.WithMaxBlockingTasks(0), ants.WithPreAlloc(true))
	if err != nil {
		panic(err)
	}
	defer gPool.Release()
	go func() {
		for {
			fmt.Println("running worker:", runtime.NumGoroutine())
			fmt.Println("pool worker:", gPool.Running())
			fmt.Println("process:", count)
			time.Sleep(time.Second)
		}
	}()
	robbinController, err = NewRobbin(flagSubWorker)
	if err != nil {
		panic(err)
	}
	robbinController.Run(handleEvent)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Hello World"))
	})
	var upgrader = websocket.Upgrader{} // use default options
	upgrader.CheckOrigin = func(r *http.Request) bool {
		return true
	}
	m.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.Host)
		err = homeTemplate.Execute(w, "ws://"+r.Host+"/echo")
		if err != nil {
			log.Fatal(err)
		}

	})
	m.HandleFunc("/long", func(w http.ResponseWriter, r *http.Request) {
		size := r.FormValue("size")
		toSize, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			fmt.Println(err.Error())
		}

		data := make([]byte, toSize)
		for i := 0; i < int(toSize); i++ {
			data[i] = 66
		}
		_, _ = w.Write(data)
	})
	m.Handle("/debug/fgprof", fgprof.Handler())
	m.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Print("upgrade:", err)
			return
		}
		defer c.Close()
		for {
			mt, message, err := c.ReadMessage()
			if err != nil {
				log.Println("read:", err)
				break
			}
			log.Printf("recv: %s", message)
			err = c.WriteMessage(mt, message)
			if err != nil {
				log.Println("write:", err)
				break
			}
		}
	})
	type WebSocketResponseJson struct {
		Seq      string `json:"seq"`
		Cmd      string `json:"cmd"`
		Response struct {
			Code    int         `json:"code"`
			CodeMsg string      `json:"codeMsg"`
			Data    interface{} `json:"data"`
		} `json:"response"`
	}

	m.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Print("upgrade:", err)
			return
		}

		_, bytesMsg, err := c.ReadMessage()
		if err != nil {
			log.Println("err:", err.Error())
			return
		}
		defer c.Close()
		respJson := &WebSocketResponseJson{}
		err = json.Unmarshal(bytesMsg, &respJson)
		if err != nil {
			log.Println("json unmarshal with err:", err.Error())
			return
		}
		respJson.Cmd = "heartbeat"
		respJson.Response.Code = 200
		respJson.Response.CodeMsg = "Success"
		err = c.WriteJSON(respJson)
		if err != nil {
			log.Println("c.WriteJSON with err:", err.Error())
			return
		}
	})

	time.Sleep(time.Hour * 24 * 30)

}
func NewFdToConns(total int) *FdToConns {
	_fdToConns := &FdToConns{total: total}
	_fdToConns.fdToConns = make([]*FdToConn, total)
	for i := 0; i < total; i++ {
		fdToConn := &FdToConn{}
		fdToConn.fdToConn = make(map[int]*Conn)
		fdToConn.mux = &sync.RWMutex{}
		_fdToConns.fdToConns[i] = fdToConn
	}
	return _fdToConns
}
func (fdToConns *FdToConns) Get(fd int) *FdToConn {
	fdToC := fdToConns.fdToConns[fd%fdToConns.total]
	return fdToC
}
func acceptNewConnection(fd int, idx int) error {
	nfd, _, err := unix.Accept(fd)
	if err != nil {
		if err == unix.EAGAIN {
			return nil
		}
		return os.NewSyscallError("accept", err)
	}
	if err := unix.SetNonblock(nfd, true); err != nil {
		return os.NewSyscallError("fcntl nonblock", err)
	}
	//conn := connPool.Get().(*Conn)
	conn := NewConn(nfd, robbinController.GetPoller())
	fdToConn := fdToConns.Get(nfd)
	fdToConn.mux.Lock()
	fdToConn.fdToConn[nfd] = conn
	fdToConn.mux.Unlock()
	//fmt.Println("accept new connection")
	_ = conn.poller.AddRead(nfd)
	//err = SetKeepAlive(nfd, 10)
	//if err != nil {
	//	return err
	//}
	return nil
}
func NewConn(fd int, poller *Poller) *Conn {
	return &Conn{opened: true, poller: poller, fd: fd, readCond: &sync.Cond{L: &sync.Mutex{}}, inboundBuffer: buffer.NewBuffer(buffer.WithPool(pool), buffer.WithEmptyError(nil)), outboundBuffer: buffer.NewBuffer(buffer.WithPool(pool), buffer.WithEmptyError(unix.EAGAIN))}
}
func (conn *Conn) init(fd int, poller *Poller) {
	conn.opened = true
	conn.poller = poller
	conn.fd = fd
	conn.readCond = &sync.Cond{L: &sync.Mutex{}}
	conn.inboundBuffer = buffer.NewBuffer(buffer.WithPool(pool), buffer.WithEmptyError(nil))
	conn.outboundBuffer = buffer.NewBuffer(buffer.WithPool(pool), buffer.WithEmptyError(unix.EAGAIN))
	conn.context = nil
	conn.buffer = nil
	conn.workerBind = false
}

//业务代码写入
func (conn *Conn) Write(data []byte) (int, error) {
	defer conn.outboundBuffer.InputStreamFinish()
	conn.outboundBuffer.ReOpenInputStream()
	err := conn.poller.ModReadWrite(conn.fd)
	if err != nil {
		fmt.Printf("fire err:%s", err.Error())
		//loopCloseConn(conn, nil)
	}
	wn, err := conn.outboundBuffer.Write(data)
	return wn, err
}

//业务代码读入
func (conn *Conn) Read(data []byte) (rd int, err error) {
	conn.readCond.L.Lock()
	//if conn.opened==false{
	//	fmt.Printf("conn:%p had closed when read data\n",conn)
	//}
	for conn.opened {
		rd, err = conn.inboundBuffer.Read(data)
		if rd != 0 || err != nil {
			break
		}
		conn.readCond.Wait()
	}
	conn.readCond.L.Unlock()
	return rd, err
}

//写入socket
func (conn *Conn) write() (int, error) {
	wnTotal := 0
	buf := bytesPool.Get().([]byte)
	defer bytesPool.Put(buf)
	for {
		//先将buf中的数据写入
		for len(conn.buffer) != 0 {
			wn, err := unix.Write(conn.fd, conn.buffer)
			if err != nil {
				if err == unix.EAGAIN {
					return wnTotal, nil
				}
				return wnTotal, err
			}
			conn.buffer = conn.buffer[wn:]
			wnTotal += wn
		}
		//读取业务写入的数据
		rd, err := conn.outboundBuffer.Read(buf)
		if err != nil {
			if err == unix.EAGAIN {
				return wnTotal, nil
			}
			return wnTotal, err
		}
		//将业务中的数据写入
		wn, err := unix.Write(conn.fd, buf[:rd])
		if err != nil {
			conn.buffer = buf[:rd]
			if err == unix.EAGAIN {
				return wnTotal, nil
			}
			return wnTotal, err
		}
		wnTotal += wn
		//如果有剩余这写入到buffer
		conn.buffer = buf[wn:rd]
	}

}

//读socket
func (conn *Conn) read() (int, error) {
	readTotal := 0
	data := bytesPool.Get().([]byte)
	defer func() {
		bytesPool.Put(data)
		conn.readCond.Signal()
	}()
	for {
		rd, err := loopRead(conn, data)
		if rd == 0 || err != nil {
			return readTotal, err
		}
		rd, _ = conn.inboundBuffer.Write(data[:rd])
		if rd != 0 {
			conn.readCond.Broadcast()
		}

		readTotal += rd
	}

}

func (worker *Worker) Start() {

	if worker.conn.context == nil {
		worker.conn.context, _ = handlerFunc.UpgradeFunc(worker.conn)
	}
	defer func() {
		worker.conn.workerBind = false
		if !worker.conn.opened {

		}
	}()
	for {
		err := handlerFunc.ServeFunc(worker.conn.context)
		if err != nil {

			//判断流中是否还有数据，没有数据退出worker
			if err == unix.EAGAIN {
				if !worker.conn.opened && !worker.conn.inboundBuffer.IsEmpty() {
					continue
				}
				return
			}
			fmt.Println(err.Error())
			return
		}
		//判断流中是否还有数据，没有数据退出worker
		if worker.conn == nil || worker.conn.opened == false || worker.conn.inboundBuffer.IsEmpty() {
			return
		}
	}
}
func (conn *Conn) releaseTCP() {
	conn.inboundBuffer = nil
	conn.outboundBuffer = nil
	conn.buffer = nil
	conn.context = nil
	conn.readCond = nil
	conn.poller = nil
}
func loopCloseConn(conn *Conn, err error) error {
	if err == nil {
		_, _ = conn.write()
		//fmt.Println("last writeln:", writeln)
	}

	if !conn.opened {
		fmt.Printf("The fd=%d in event-loop() is already closed, skipping it\n", conn.fd)
		return nil
	}

	err0, err1 := conn.poller.Delete(conn.fd), unix.Close(conn.fd)
	if err0 == nil && err1 == nil {
		fdToConn := fdToConns.Get(conn.fd)
		fdToConn.mux.Lock()
		_conn := fdToConn.fdToConn[conn.fd]
		if _conn.opened == false {
			delete(fdToConn.fdToConn, conn.fd)
		}
		fdToConn.mux.Unlock()
		//el.calibrateCallback(el, -1)
		//if el.eventHandler.OnClosed(c, err) == Shutdown {
		//	return errors.ErrServerShutdown
		//}
		conn.opened = false
		conn.readCond.Broadcast()
		//connPool.Put(conn)
	} else {
		if err0 != nil {
			fmt.Printf("Failed to delete fd=%d from poller in event-loop, %v", conn.fd, err0)
		}
		if err1 != nil {
			fmt.Printf("Failed to close fd=%d in event-loop(), %v",
				conn.fd, os.NewSyscallError("close", err1))
		}
	}

	return nil
}

func (buf *ResponseBuffer) Close() error {
	buf.Buffer = nil
	return nil
}
func (response *Response) WriteStatus(status string) {
	response.status = status
}
func (response *Response) WriteStatusCode(statusCode int) {
	response.statusCode = statusCode
}
func (response *Response) WriteString(datas string) {
	response.len += len(datas)
	response.buffer.Write([]byte(datas))
}
func (response *Response) WriteBytes(datas []byte) {
	response.len += len(datas)
	response.buffer.Write(datas)
}
func NewResponse() *Response {
	return &Response{statusCode: 200, status: "200 OK", buffer: &ResponseBuffer{Buffer: &bytes.Buffer{}}}
}
func (response *Response) GetLen() int {
	return response.len
}
func NewRobbin(numSubPoller int) (*Robbin, error) {
	robbin := &Robbin{pollers: make([]*Poller, numSubPoller), total: numSubPoller}
	for i := 0; i < numSubPoller; i++ {
		subPoller, err := OpenPoller()
		if err != nil {
			return nil, err
		}
		robbin.pollers[i] = subPoller
	}
	return robbin, nil
}
func (robbin *Robbin) GetPoller() *Poller {
	poller := robbin.pollers[robbin.curIdx]
	robbin.curIdx = (robbin.curIdx + 1) % robbin.total
	return poller
}

func NewServer(network string, addr string, numListener int) *Server {
	return &Server{network: network, addr: addr, numListener: numListener}
}
func (server *Server) Serve() error {
	for i := 0; i < server.numListener; i++ {
		listenerFd, _, err := tcpReusablePort(server.network, server.addr, true)
		if err != nil {
			panic(err)
		}
		go func(idx int, fd int) {
			poller, err := OpenPoller()
			if err != nil {
				panic(err)
			}
			_ = poller.AddRead(fd)
			err = startMainLoop(poller, idx)
			if err != nil {
				panic(err)
			}
		}(i, listenerFd)
	}
	return nil
}
func (handler *HttpHandler) UpgradeFunc(conn *Conn) (context interface{}, err error) {
	reader := bufio.NewReader(conn)
	rw := bufio.NewReadWriter(reader, bufio.NewWriter(conn))
	return &HttpContext{reader: reader, conn: conn, rw: rw}, nil
}

func (handler *HttpHandler) ServeFunc(context interface{}) error {
	//defer fmt.Println("process serveFuc complete")
	atomic.AddInt64(&count, 1)

	//fmt.Println("process serveFuc")
	ctx := context.(*HttpContext)
	_, err := http.ReadRequest(ctx.reader)
	if err != nil {
		panic(err)
		return err
	}
	resp := NewResponse()
	httpResp := http.Response{}
	httpResp.StatusCode = resp.statusCode
	httpResp.Status = resp.status
	//httpResp.Proto = "HTTP/1.0"
	httpResp.ProtoMajor = 1
	httpResp.ProtoMinor = 0
	httpResp.Header = make(http.Header)
	httpResp.Header["Content-Type"] = []string{"text/plain; charset=utf-8"}
	resp.WriteString("ok")
	//for i := 0; i < 4096; i++ {
	//	resp.WriteString("ok")
	//}
	resp.WriteString("\r\n")
	httpResp.ContentLength = int64(resp.buffer.Len())
	httpResp.Body = resp.buffer
	err = httpResp.Write(ctx.conn)
	if err != nil {
		fmt.Println(err.Error())
		return err
	}
	//http.ReadResponse()
	//res := response.NewResponse(req, ctx.conn, ctx.rw)
	//handler.ServeHTTP(res, req)
	//res.FinishRequest()
	//response.FreeResponse(res)

	return nil
}
func (handler *TcpHandler) UpgradeFunc(conn *Conn) (context interface{}, err error) {
	return &TcpContext{reader: bufio.NewReader(conn), conn: conn}, nil
}
func (handler *TcpHandler) ServeFunc(context interface{}) error {
	ctx := context.(*TcpContext)
	data := make([]byte, 50)
	remain := 50
	for {
		rd, err := ctx.reader.Read(data)
		if err != nil {
			return err
		}
		remain -= rd
		if remain != 0 {
			continue
		}
		break
	}
	//fmt.Println("read:", string(data))
	_, _ = ctx.conn.Write([]byte("ok"))
	return nil
}
func (conn *Conn) Close() error {
	return errors.New("not implement")
}

// LocalAddr returns the local network address.
func (conn *Conn) LocalAddr() net.Addr {
	return nil
}

// RemoteAddr returns the remote network address.
func (conn *Conn) RemoteAddr() net.Addr {
	return nil
}
func (conn *Conn) SetDeadline(t time.Time) error {
	return errors.New("not implement")
}

// SetReadDeadline sets the deadline for future Read calls
// and any currently-blocked Read call.
// A zero value for t means Read will not time out.
func (conn *Conn) SetReadDeadline(t time.Time) error {
	return errors.New("not implement")
}

// SetWriteDeadline sets the deadline for future Write calls
// and any currently-blocked Write call.
// Even if write times out, it may return n > 0, indicating that
// some of the data was successfully written.
// A zero value for t means Write will not time out.
func (conn *Conn) SetWriteDeadline(t time.Time) error {
	return errors.New("not implement")
}

func (handler *WebsocketHandler) UpgradeFunc(conn *Conn) (context interface{}, err error) {
	reader := bufio.NewReader(conn)
	rw := bufio.NewReadWriter(reader, bufio.NewWriter(conn))
	return &WebsocketContext{reader: reader, conn: conn, rw: rw}, nil
}

func (handler *WebsocketHandler) ServeFunc(context interface{}) error {
	ctx := context.(*WebsocketContext)
	req, err := http.ReadRequest(ctx.reader)
	if err != nil {
		return err
	}
	res := response.NewResponse(req, ctx.conn, ctx.rw)
	m.ServeHTTP(res, req)
	res.FinishRequest()
	response.FreeResponse(res)
	return nil
}

var homeTemplate = template.Must(template.New("").Parse(`
<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<script>  
window.addEventListener("load", function(evt) {
    var output = document.getElementById("output");
    var input = document.getElementById("input");
    var ws;
    var print = function(message) {
        var d = document.createElement("div");
        d.textContent = message;
        output.appendChild(d);
    };
    document.getElementById("open").onclick = function(evt) {
        if (ws) {
            return false;
        }
        ws = new WebSocket("{{.}}");
        ws.onopen = function(evt) {
            print("OPEN");
        }
        ws.onclose = function(evt) {
            print("CLOSE");
            ws = null;
        }
        ws.onmessage = function(evt) {
            print("RESPONSE: " + evt.data);
        }
        ws.onerror = function(evt) {
            print("ERROR: " + evt.data);
        }
        return false;
    };
    document.getElementById("send").onclick = function(evt) {
        if (!ws) {
            return false;
        }
        print("SEND: " + input.value);
        ws.send(input.value);
        return false;
    };
    document.getElementById("close").onclick = function(evt) {
        if (!ws) {
            return false;
        }
        ws.close();
        return false;
    };
});
</script>
</head>
<body>
<table>
<tr><td valign="top" width="50%">
<p>Click "Open" to create a connection to the server, 
"Send" to send a message to the server and "Close" to close the connection. 
You can change the message and send multiple times.
<p>
<form>
<button id="open">Open</button>
<button id="close">Close</button>
<p><input id="input" type="text" value="Hello world!">
<button id="send">Send</button>
</form>
</td><td valign="top" width="50%">
<div id="output"></div>
</td></tr></table>
</body>
</html>
`))
