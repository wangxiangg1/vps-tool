package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	web "vps-tool/control-plane"
	"vps-tool/control-plane/internal/actions"
	"vps-tool/control-plane/internal/auth"
	"vps-tool/control-plane/internal/backups"
	"vps-tool/control-plane/internal/config"
	"vps-tool/control-plane/internal/db"
	"vps-tool/control-plane/internal/gateway"
	"vps-tool/control-plane/internal/httpapi"
	"vps-tool/control-plane/internal/nodes"
	"vps-tool/control-plane/internal/scheduler"
)

func main() {
	health := flag.Bool("health", false, "check the running server and exit")
	flag.Parse()
	if *health {
		addr := os.Getenv("VPS_TOOL_LISTEN_ADDR")
		if addr == "" {
			addr = ":8000"
		}
		if err := checkHealth(addr); err != nil {
			log.Fatal(err)
		}
		return
	}
	cfg, e := config.Load()
	if e != nil {
		log.Fatal(e)
	}
	database, e := db.Open(cfg.DBPath)
	if e != nil {
		log.Fatal(e)
	}
	defer database.Close()
	if e = database.Init(context.Background()); e != nil {
		log.Fatal(e)
	}
	if e = auth.EnsureAdmin(database, cfg); e != nil {
		log.Fatal(e)
	}
	ns := &nodes.Service{DB: database, Cfg: cfg}
	as := &actions.Service{DB: database, Cfg: cfg, Nodes: ns}
	gw := gateway.New(cfg, ns)
	as.GW = gw
	gw.Actions = as
	bs := &backups.Service{DB: database, Cfg: cfg, Nodes: ns}
	ts := &scheduler.TaskService{DB: database, Nodes: ns, Actions: as}
	sch := &scheduler.Scheduler{DB: database, Tasks: ts, Actions: as, Cfg: cfg}
	sub, e := fs.Sub(web.Files, "web")
	if e != nil {
		log.Fatal(e)
	}
	app := &httpapi.Server{DB: database, Cfg: cfg, Nodes: ns, Actions: as, Gateway: gw, Backups: bs, Tasks: ts, Static: http.FileServer(http.FS(sub)), Limiter: auth.NewLimiter()}
	mux := http.NewServeMux()
	mux.Handle("/agent", http.HandlerFunc(gw.Handler))
	mux.Handle("/api/agent/xui-backups/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app.AgentBackup(w, r, r.URL.Path[len("/api/agent/xui-backups/"):])
	}))
	mux.Handle("/", app)
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		log.Printf("vps-server listening on %s", cfg.ListenAddr)
		if e := srv.ListenAndServe(); e != nil && e != http.ErrServerClosed {
			log.Fatal(e)
		}
	}()
	ctx, stopScheduler := context.WithCancel(context.Background())
	schedulerDone := make(chan struct{})
	go func() { defer close(schedulerDone); sch.Run(ctx) }()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	stopScheduler()
	<-schedulerDone
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

func checkHealth(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + net.JoinHostPort(host, port) + "/api/health")
	if err != nil {
		return fmt.Errorf("health request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health returned HTTP %d", response.StatusCode)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || !result.OK {
		return fmt.Errorf("server is not healthy")
	}
	return nil
}
