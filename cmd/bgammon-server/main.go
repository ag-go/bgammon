package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
)

func main() {
	var (
		tcpAddress     string
		wsAddress      string
		debug          int
		rollStatistics bool
	)
	flag.StringVar(&tcpAddress, "tcp", "localhost:1337", "TCP listen address")
	flag.StringVar(&wsAddress, "ws", "localhost:1338", "WebSocket listen address")
	flag.IntVar(&debug, "debug", 0, "print debug information and serve pprof on specified port")
	flag.BoolVar(&rollStatistics, "statistics", false, "print dice roll statistics and exit")
	flag.Parse()

	if rollStatistics {
		printRollStatistics()
		return
	}

	if tcpAddress == "" && wsAddress == "" {
		log.Fatal("Error: A TCP and/or WebSocket listen address must be specified.")
	}

	if debug > 0 {
		go func() {
			log.Fatal(http.ListenAndServe(fmt.Sprintf("localhost:%d", debug), nil))
		}()
	}

	s := newServer()
	if tcpAddress != "" {
		s.listen("tcp", tcpAddress)
	}
	if wsAddress != "" {
		s.listen("ws", wsAddress)
	}
	select {}
}

func printRollStatistics() {
	var oneSame, doubles int
	var lastroll1, lastroll2 int

	total := 1000000
	for i := 0; i < total; i++ {
		roll1 := randInt(6) + 1
		roll2 := randInt(6) + 1

		if roll1 == lastroll1 || roll1 == lastroll2 || roll2 == lastroll1 || roll2 == lastroll2 {
			oneSame++
		}

		if roll1 == roll2 {
			doubles++
		}

		lastroll1, lastroll2 = roll1, roll2
	}

	log.Printf("total: %d, one same: %d (%.0f%%), doubles: %d (%.0f%%)", total, oneSame, float64(oneSame)/float64(total)*100, doubles, float64(doubles)/float64(total)*100)
}
