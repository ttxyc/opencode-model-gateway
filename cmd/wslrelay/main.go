package main

import (
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8788", "listen address")
	targetHost := flag.String("host", "", "Windows host IP (default: WSL default gateway)")
	targetPort := flag.Int("port", 8788, "Windows gateway port")
	flag.Parse()
	host := *targetHost
	if host == "" {
		out, err := exec.Command("sh", "-c", "ip route | awk '/^default/ {print $3; exit}'").Output()
		if err != nil {
			log.Fatal(err)
		}
		host = strings.TrimSpace(string(out))
	}
	if host == "" {
		log.Fatal("cannot determine Windows host IP")
	}
	addr := net.JoinHostPort(host, itoa(*targetPort))
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("wsl relay %s -> %s", *listen, addr)
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handle(conn, addr)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func handle(src net.Conn, addr string) {
	defer src.Close()
	dst, err := net.Dial("tcp", addr)
	if err != nil {
		return
	}
	defer dst.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(dst, src); done <- struct{}{} }()
	go func() { _, _ = io.Copy(src, dst); done <- struct{}{} }()
	<-done
}
