package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"xupay/internal/platform"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	static := os.Getenv("STATIC_DIR")
	if static == "" {
		static = "webui"
	}
	app, e := platform.New(ctx, os.Getenv("DATABASE_URL"), os.Getenv("MASTER_KEY"), static, os.Getenv("COOKIE_SECURE") == "true")
	if e != nil {
		log.Fatal(e)
	}
	defer app.DB.Close()
	go app.RunMaintenance(ctx)
	go app.RunCheckout(ctx)
	go app.RunPayments(ctx)
	go app.RunRenewals(ctx)
	go app.RunProxyChecks(ctx)
	go app.RunProviderDiagnostics(ctx)
	port := os.Getenv("PORT")
	if port == "" {
		port = "18860"
	}
	host := os.Getenv("LISTEN_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	server := &http.Server{Addr: host + ":" + port, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(c)
	}()
	log.Printf("XUPAY listening on %s", server.Addr)
	if e = server.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		log.Fatal(e)
	}
}
