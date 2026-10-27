// mpts2spts - low-overhead MPTS -> SPTS splitter with a web UI.
//
// One goroutine per input joins the MPTS once; each TS packet costs one PID
// table lookup plus a copy per output that wants it. PAT, PMT and SDT are
// regenerated per output from values frozen in SQLite, so an output looks
// the same after every restart.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// version is set at build time: -ldflags "-X main.version=1.2.3"
var version = "dev"

func loadEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		os.Setenv(k, v) // file wins: desktop sessions already export USERNAME
	}
}

func main() {
	listen := flag.String("listen", ":8080", "web UI listen address")
	dbPath := flag.String("db", "mpts2spts.db", "SQLite database file")
	envPath := flag.String("env", ".env", "file with USERNAME and PASSWORD")
	pprofAddr := flag.String("pprof", "", "debug: serve Go profiler on this address (e.g. 127.0.0.1:6060)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("mpts2spts", version)
		return
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	loadEnv(*envPath)
	user, pass := os.Getenv("USERNAME"), os.Getenv("PASSWORD")
	if user == "" || pass == "" {
		log.Fatal("USERNAME and PASSWORD must be set (in the environment or the -env file)")
	}

	store, err := OpenStore(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	engine := NewEngine()
	app := NewApp(store, engine, user, pass)
	if err := app.Apply(); err != nil {
		log.Fatal(err)
	}

	if *pprofAddr != "" {
		go http.ListenAndServe(*pprofAddr, nil) // DefaultServeMux carries only pprof
	}
	srv := &http.Server{Addr: *listen, Handler: app.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("mpts2spts %s, web UI on %s", version, *listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Print("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	srv.Shutdown(ctx)
	cancel()
	engine.StopAll()
}
