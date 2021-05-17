package test

import (
	"fmt"
	"io"
	"net"
	"testing"
)

func TestDail(t *testing.T) {
	dailClient, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = dailClient.Write([]byte("GET / HTTP/1.0\nHost: 127.0.0.1:8080\nUser-Agent: ApacheBench/2.3\nAccept: */*\nConnection: keep-alive\r\n\r\n")[:50])
		if err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 4096)
		for {
			rd, err := dailClient.Read(data)
			if err != nil {
				if err == io.EOF {
					break
				}
				t.Fatal(err)
			}
			if rd == 2 {
				fmt.Println("break")
				break
			}
			fmt.Println(string(data[:rd]))
		}
	}

}
