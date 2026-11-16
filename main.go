package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type config struct {
	Addr, PublicURL, DataDir string
	RegistrationCooldown     time.Duration
	TrustedProxies           []netip.Prefix
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func loadConfig() (config, error) {
	c := config{Addr: env("LISTEN_ADDR", "127.0.0.1:8080"), PublicURL: strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/"), DataDir: env("DATA_DIR", "data")}
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return c, errors.New("PUBLIC_URL must be an http(s) origin without a path")
	}
	c.RegistrationCooldown, err = time.ParseDuration(env("REGISTRATION_COOLDOWN", "1h"))
	if err != nil || c.RegistrationCooldown < time.Second {
		return c, errors.New("REGISTRATION_COOLDOWN must be at least 1s")
	}
	for _, s := range strings.Split(os.Getenv("TRUSTED_PROXIES"), ",") {
		if strings.TrimSpace(s) == "" {
			continue
		}
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			return c, fmt.Errorf("TRUSTED_PROXIES: %w", err)
		}
		c.TrustedProxies = append(c.TrustedProxies, p)
	}
	c.DataDir, err = filepath.Abs(c.DataDir)
	return c, err
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	command, err := parseCLI(os.Args[1:], os.Stdout)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := openDB(filepath.Join(cfg.DataDir, "index.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	a, err := newApp(db, cfg)
	if err != nil {
		return err
	}
	if command != nil {
		return a.runUserCommand(*command, os.Getenv("ACCOUNT_PASSWORD"), os.Stdin, os.Stdout)
	}
	srv := &http.Server{Addr: cfg.Addr, Handler: a.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	go a.cleanup(ctx)
	log.Printf("Home: Equestria mods: %s (listening on %s)", cfg.PublicURL, cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *app) cleanup(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		now := time.Now().Unix()
		for _, table := range []string{"sessions", "tokens", "limits"} {
			if _, err := a.db.Exec("DELETE FROM "+table+" WHERE expires_at <= ?", now); err != nil {
				log.Printf("cleanup %s: %v", table, err)
			}
		}
		_, err := a.db.Exec("DELETE FROM activity WHERE created_at < ?", now-int64((7*24*time.Hour).Seconds()))
		if err != nil {
			log.Printf("cleanup activity: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func randomToken() string { return rand.Text() }

func (a *app) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	ip = ip.Unmap()
	trusted := func(ip netip.Addr) bool {
		for _, p := range a.cfg.TrustedProxies {
			if p.Contains(ip) {
				return true
			}
		}
		return false
	}
	if trusted(ip) {
		chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		for i := len(chain) - 1; i >= 0 && trusted(ip); i-- {
			next, err := netip.ParseAddr(strings.TrimSpace(chain[i]))
			if err != nil {
				break
			}
			ip = next.Unmap()
		}
	}
	if ip.Is6() {
		return netip.PrefixFrom(ip, 64).Masked().String()
	}
	return ip.String()
}

func (a *app) limit(key string, max int, window time.Duration) error {
	var count, expires int64
	err := a.db.QueryRow(`INSERT INTO limits(key,count,expires_at) VALUES(?,1,?) ON CONFLICT(key) DO UPDATE SET count=CASE WHEN expires_at<=? THEN 1 ELSE count+1 END, expires_at=CASE WHEN expires_at<=? THEN excluded.expires_at ELSE expires_at END RETURNING count,expires_at`, key, time.Now().Add(window).Unix(), time.Now().Unix(), time.Now().Unix()).Scan(&count, &expires)
	if err != nil {
		return err
	}
	if count > int64(max) {
		return &apiError{http.StatusTooManyRequests, "Too many attempts. Try again later.", int(max64(1, expires-time.Now().Unix()))}
	}
	return nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
func idParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, problem(400, "Invalid ID.")
	}
	return id, nil
}
