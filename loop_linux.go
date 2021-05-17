package main

import (
	"fmt"
	"io"

	"golang.org/x/sys/unix"
)

func startMainLoop(poller *Poller, idx int) error {
	err := poller.Polling(func(fd int, ev uint32) error {
		err := acceptNewConnection(fd, idx)
		if err != nil {
			fmt.Println(err.Error())
		}
		return err
	})
	return err
}
func (robbin *Robbin) Run(handleEvent func(fd int, ev uint32) error) {
	for i := 0; i < robbin.total; i++ {
		go func(idx int) {
			//	runtime.LockOSThread()
			err := robbin.pollers[idx].Polling(handleEvent)
			if err != nil {
				panic(err)
			}
		}(i)
	}
}
func handleEvent(fd int, ev uint32) error {
	fdToConn:=fdToConns.Get(fd)
	fdToConn.mux.RLock()
	conn := fdToConn.fdToConn[fd]
	if conn == nil {
		fmt.Println("conn is nil")
		fdToConn.mux.RUnlock()
		return nil
	}
	fdToConn.mux.RUnlock()

	if ev&OutEvents != 0 {
		//fmt.Println("filter write")
		_, err := conn.write()
		if err != nil {
			if err == io.EOF {
				_ = conn.poller.ModRead(conn.fd)
				if !conn.outboundBuffer.IsEmpty() {
					_ = conn.poller.ModReadWrite(conn.fd)
				}
			} else {
//			    fmt.Printf("write trigger close conn reason:%v,ev:%v\n",err,ev)
				return loopCloseConn(conn, err)
			}

		}
	}
	if ev&InEvents != 0 {
		//fmt.Println("filter read")
		rd, err := conn.read()
		if rd == 0 || err != nil {
			//fmt.Printf("read conn:%p ev: with err %v,exec close conn", conn,ev,err)
			//go func() {
			if err!=unix.EAGAIN{
//				fmt.Printf("read trigger close conn:%p reason:%v,ev:%v,rd:%v\n",conn,err,ev,rd)
				return loopCloseConn(conn, err)
			}

			//}()
		}

		if conn.workerBind == false && !conn.inboundBuffer.IsEmpty() {
			err = gPool.Submit(func() {
				worker := &Worker{conn: conn}
				conn.workerBind = true
				worker.Start()
			})
			if err != nil {
				panic(err)
			}
		}

	}
	//if ev&0x8!=0 || ev&0x10!=0{
	//	fmt.Printf("conn:%p ev:%d", conn,ev)
	//	loopCloseConn(conn, errors.New(""))
	//	return nil
	//}

	return nil
}
func loopRead(conn *Conn, data []byte) (n int, err error) {
	rd, err := unix.Read(conn.fd, data)
	if rd == 0 || err != nil {
		if err == unix.EAGAIN {
			return 0, nil
		}
		return 0, err
	}
	return rd, nil
}
