package main

import (
	"log"
	"time"

	"github.com/chokeslam46/go-electrum/electrum"
)

func main() {
	server := electrum.NewServer(&electrum.DefaultServerOptions)
	if err := server.ConnectTCP("bch.imaginary.cash:50001"); err != nil {
		log.Fatal(err)
	}

	serverVer, protocolVer, err := server.ServerVersion()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Server version: %s [Protocol %s]", serverVer, protocolVer)

	go func() {
		for {
			if err := server.Ping(); err != nil {
				log.Fatal(err)
			}
			time.Sleep(60 * time.Second)
		}
	}()

	defer server.Shutdown()
}
