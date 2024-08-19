package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
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
	defaultNetwork = "Arabica"
	blockTime      = 11.3
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
	Networks []Network `json:"networks"`
	Port     int       `json:"port"`
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
}

func NewServer(cfg *Config) *Server {
	return &Server{cfg: cfg}
}

func (s *Server) Start(ctx context.Context) error {
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
	networkName := defaultNetwork
	statusCheck := false
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) == 2 && len(parts[1]) > 0 {
		networkName = parts[1]
	} else if len(parts) == 3 && parts[2] == "status" {
		statusCheck = true
	}

	// Find the matching network in the config
	var selectedNetwork *Network
	for _, network := range s.cfg.Networks {
		if strings.EqualFold(network.Name, networkName) {
			selectedNetwork = &network
			break
		}
	}

	// If no matching network is found, return a 404 error
	if selectedNetwork == nil {
		http.NotFound(w, r)
		return
	}

	// Update the RPC client to use the selected network's RPC
	rpcClient, err := rpc.New(selectedNetwork.RPC, "/websocket")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if s.shouldUpdateHeight() {
		status, err := rpcClient.Status(r.Context())
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

	blocksRemaining := selectedNetwork.UpgradeHeight - int(s.height)
	timeLeft := float64(blocksRemaining) * blockTime

	data := struct {
		NetworkName   string
		TimeLeft      int
		CurrentHeight int
		UpgradeHeight int
	}{
		NetworkName:   selectedNetwork.Name,
		TimeLeft:      int(timeLeft),
		CurrentHeight: int(s.height),
		UpgradeHeight: selectedNetwork.UpgradeHeight,
	}
	err = tmpl.Execute(w, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) shouldUpdateHeight() bool {
	return s.height == 0 || time.Since(s.lastUpdate) > refreshRate
}
