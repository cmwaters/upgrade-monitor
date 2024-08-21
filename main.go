package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"

	rpc "github.com/tendermint/tendermint/rpc/client/http"
)

const (
	defaultNetwork = "Mocha"
	refreshRate    = 10 * time.Second
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := Run(ctx); err != nil {
		log.Fatal(err)
	}
}

func Run(ctx context.Context) error {
	dir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	config, err := ReadConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		config, err = ReadConfig("config.json")
		if err != nil {
			return err
		}
	}
	server := NewServer(config)
	return server.Start(ctx)
}

//go:embed templates/*
var content embed.FS

type Config struct {
	Network Network `json:"network"`
	Port    int     `json:"port"`
}

type Network struct {
	Name          string `json:"name"`
	RPC           string `json:"rpc"`
	UpgradeHeight int    `json:"upgrade_height"`
}

func ReadConfig(path string) (*Config, error) {
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config Config
	err = json.Unmarshal(file, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}

type Server struct {
	cfg        *Config
	height     int64
	lastUpdate time.Time
	blockRate  float64
	rpc        *rpc.HTTP
}

func NewServer(cfg *Config) *Server {
	return &Server{cfg: cfg}
}

func (s *Server) Start(ctx context.Context) error {
	rpcClient, err := rpc.New(s.cfg.Network.RPC, "/websocket")
	if err != nil {
		return err
	}
	s.rpc = rpcClient

	status, err := s.rpc.Status(ctx)
	if err != nil {
		return err
	}
	lastHeight := status.SyncInfo.LatestBlockHeight
	lastTime := status.SyncInfo.LatestBlockTime
	earliestHeight := status.SyncInfo.EarliestBlockHeight
	if earliestHeight < lastHeight-10000 {
		earliestHeight = lastHeight - 10000
	}
	header, err := s.rpc.Header(ctx, &earliestHeight)
	if err != nil {
		return err
	}
	earliestTime := header.Header.Time
	s.blockRate = math.Round((float64(lastTime.Sub(earliestTime).Seconds())/float64(lastHeight-earliestHeight))*100) / 100
	s.height = lastHeight
	s.lastUpdate = lastTime

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.cfg.Port),
		Handler: http.HandlerFunc(s.handleRequest),
	}

	go func() {
		<-ctx.Done()
		if err := httpServer.Shutdown(context.Background()); err != nil {
			log.Printf("Error shutting down the server: %v", err)
		}
	}()

	log.Printf("Server is starting on port %s", httpServer.Addr)
	if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}

	return nil
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	statusCheck := false
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) == 2 && parts[1] == "status" {
		statusCheck = true
	}

	if s.shouldUpdateHeight() {
		status, err := s.rpc.Status(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.height = status.SyncInfo.LatestBlockHeight
		s.lastUpdate = time.Now()
	}

	if statusCheck {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int64{
			"current_height": s.height,
		})
		return
	}

	tmpl, err := template.ParseFS(content, "templates/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	blocksRemaining := s.cfg.Network.UpgradeHeight - int(s.height)
	timeLeft := float64(blocksRemaining) * s.blockRate

	data := struct {
		NetworkName   string
		TimeLeft      int
		CurrentHeight int
		UpgradeHeight int
		BlockRate     float64
	}{
		NetworkName:   s.cfg.Network.Name,
		TimeLeft:      int(timeLeft),
		CurrentHeight: int(s.height),
		UpgradeHeight: s.cfg.Network.UpgradeHeight,
		BlockRate:     s.blockRate,
	}
	err = tmpl.Execute(w, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) shouldUpdateHeight() bool {
	return s.height == 0 || time.Since(s.lastUpdate) > refreshRate
}
