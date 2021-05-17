// +build freebsd dragonfly darwin

package main

import (
	"fmt"
	"io"

	"golang.org/x/sys/unix"
)

func startMainLoop(poller *Poller, idx int) error {
	err := poller.Polling(func(fd int, filter int16) error {
		err := acceptNewConnection(fd, idx)
		if err != nil {
			fmt.Println(err.Error())
		}
		return err
	})
	return err
}
func (robbin *Robbin) Run(handleEvent func(fd int, filter int16) error) {
	for i := 0; i < robbin.total; i++ {
		go func(idx int) {
			err := robbin.pollers[idx].Polling(handleEvent)
			if err != nil {
				panic(err)
			}
		}(i)
	}
}
func handleEvent(fd int, filter int16) error {

	fdToConn:=fdToConns.Get(fd)
	fdToConn.mux.RLock()
	conn := fdToConn.fdToConn[fd]
	if conn == nil {
		fmt.Println("conn is nil")
		fdToConn.mux.RUnlock()
		return nil
	}
	fdToConn.mux.RUnlock()
	if filter == EVFilterSock {
		//	fmt.Println("filter close")
		_ = loopCloseConn(conn, nil)
		return nil
	}
	if filter == EVFilterWrite {
		_, err := conn.write()
		if err != nil {
			if err == io.EOF {
				_ = conn.poller.ModRead(conn.fd)
				if !conn.outboundBuffer.IsEmpty() {
					_ = conn.poller.ModReadWrite(conn.fd)
				}
			} else {
				fmt.Println(err.Error())
				return err
			}

		}
	}

	if filter == EVFilterRead {
		//fmt.Println("filter read")
		_, err := conn.read()
		if err != nil {
			if err!=unix.EAGAIN{
				return err
			}
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
	return nil
}
func loopRead(conn *Conn, data []byte) (n int, err error) {
	rd, err := unix.Read(conn.fd, data)
	if err != nil {
		return rd, err
	}
	return rd, nil
}
