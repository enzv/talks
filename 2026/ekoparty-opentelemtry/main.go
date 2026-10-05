package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	defaultPort        = "8080"
	defaultConfigURL   = "http://127.0.0.1:18090"
	shipmentID         = "SHP-9182"
	scenarioSocketPath = "/tmp/trace-lab-config-server.sock"
)

type serviceConfig struct {
	RoutingURL      string
	NotificationURL string
	InternalKey     string
}

type service struct {
	name   string
	config serviceConfig
	client *http.Client
	logger *slog.Logger
}

type configDocument struct {
	Name            string           `json:"name"`
	Profiles        []string         `json:"profiles"`
	Label           string           `json:"label"`
	Version         string           `json:"version"`
	PropertySources []propertySource `json:"propertySources"`
}

type propertySource struct {
	Name   string            `json:"name"`
	Source map[string]string `json:"source"`
}

type vehiclePosition struct {
	VehicleID  string  `json:"vehicle_id"`
	ShipmentID string  `json:"shipment_id"`
	Status     string  `json:"status"`
	Zone       string  `json:"zone"`
	Latitude   float64 `json:"latitude"`
	Longitude  float64 `json:"longitude"`
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func main() {
	if len(os.Args) != 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "shipment-api":
		err = runShipmentAPI()
	case "routing-api":
		err = runRoutingAPI()
	case "tracking-api":
		err = runTrackingAPI()
	case "notification-api":
		err = runNotificationAPI()
	case "config-server":
		err = runConfigServer()
	case "scenario-read":
		err = runScenarioRead()
	case "scenario-replan":
		err = runScenarioReplan()
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: trace-lab {shipment-api|routing-api|tracking-api|notification-api|config-server|scenario-read|scenario-replan}")
}

func runShipmentAPI() error {
	logger := newLogger("shipment-api")
	config, err := bootstrapConfig(context.Background(), "shipment-api", logger)
	if err != nil {
		return err
	}

	s := &service{
		name:   "shipment-api",
		config: config,
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/v1/shipments/plan", s.planShipment)
	mux.Handle("/internal/shipments/", requireInternal(s, http.HandlerFunc(s.replanShipment)))
	return serve(s.name, mux, s.logger)
}

func runRoutingAPI() error {
	logger := newLogger("routing-api")
	config, err := bootstrapConfig(context.Background(), "routing-api", logger)
	if err != nil {
		return err
	}

	s := &service{
		name:   "routing-api",
		config: config,
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/v1/routes/plan", s.planRoute)
	mux.HandleFunc("/v1/routes/replan", s.replanRoute)
	mux.Handle("/internal/routes/active", requireInternal(s, http.HandlerFunc(s.activeRoutes)))
	return serve(s.name, mux, s.logger)
}

func runTrackingAPI() error {
	logger := newLogger("tracking-api")
	config, err := bootstrapConfig(context.Background(), "tracking-api", logger)
	if err != nil {
		return err
	}

	db, err := openDatabase(context.Background(), logger)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := initializeDatabase(context.Background(), db); err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}

	s := &service{
		name:   "tracking-api",
		config: config,
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/v1/track/", func(w http.ResponseWriter, r *http.Request) {
		s.trackShipment(w, r, db)
	})
	mux.Handle("/internal/fleet/positions", requireInternal(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.fleetPositions(w, r, db)
	})))
	return serve(s.name, mux, s.logger)
}

func runNotificationAPI() error {
	logger := newLogger("notification-api")
	config, err := bootstrapConfig(context.Background(), "notification-api", logger)
	if err != nil {
		return err
	}

	s := &service{
		name:   "notification-api",
		config: config,
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.Handle("/v1/notifications", requireInternal(s, http.HandlerFunc(s.createNotification)))
	return serve(s.name, mux, s.logger)
}

func runConfigServer() error {
	logger := newLogger("config-server")
	internalKey := os.Getenv("INTERNAL_KEY")
	if internalKey == "" {
		return fmt.Errorf("INTERNAL_KEY is required")
	}

	s := &service{
		name: "config-server",
		config: serviceConfig{
			InternalKey: internalKey,
		},
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		configHandler(w, r, s.config.InternalKey)
	})
	control, err := listenScenarioControl()
	if err != nil {
		return err
	}
	defer func() {
		if err := control.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			s.logger.Error("scenario_control_close_failed", "error", err)
		}
		if err := os.Remove(scenarioSocketPath); err != nil && !os.IsNotExist(err) {
			s.logger.Error("scenario_control_cleanup_failed", "error", err)
		}
	}()
	go serveScenarioControl(control, s.logger, s.config.InternalKey)

	return serve(s.name, mux, s.logger)
}

func runScenarioRead() error {
	return triggerScenario("read")
}

func runScenarioReplan() error {
	return triggerScenario("replan")
}

type scenarioResult struct {
	Scenario   string `json:"scenario"`
	Status     string `json:"status"`
	Downstream string `json:"downstream"`
	Error      string `json:"error,omitempty"`
}

func executeScenario(name string, logger *slog.Logger, internalKey string) scenarioResult {
	result := scenarioResult{Scenario: name, Status: "failed"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 10 * time.Second}
	var err error
	switch name {
	case "read":
		result.Downstream = "tracking-api"
		endpoint := strings.TrimRight(envOrDefault("TRACKING_URL", "http://tracking-api.dispatch-core.svc.cluster.local:8080"), "/") + "/internal/fleet/positions"
		logger.Info("scenario_started", "scenario", name, "downstream", result.Downstream)
		err = callJSON(ctx, client, http.MethodGet, endpoint, internalKey, nil, nil)
	case "replan":
		result.Downstream = "shipment-api"
		endpoint := strings.TrimRight(envOrDefault("SHIPMENT_URL", "http://shipment-api.dispatch-core.svc.cluster.local:8080"), "/") + "/internal/shipments/" + url.PathEscape(shipmentID) + "/replan"
		logger.Info("scenario_started", "scenario", name, "downstream", result.Downstream)
		err = callJSON(ctx, client, http.MethodPost, endpoint, internalKey, map[string]string{"reason": "dispatch exception"}, nil)
	default:
		result.Error = "unknown scenario"
		return result
	}
	if err != nil {
		result.Error = err.Error()
		logger.Error("scenario_failed", "scenario", name, "downstream", result.Downstream, "error", err)
		return result
	}

	result.Status = "completed"
	logger.Info("scenario_completed", "scenario", name, "downstream", result.Downstream)
	return result
}

func triggerScenario(name string) error {
	connection, err := net.DialTimeout("unix", scenarioSocketPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect to scenario control: %w", err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(35 * time.Second)); err != nil {
		return fmt.Errorf("set scenario control deadline: %w", err)
	}
	if _, err := fmt.Fprintln(connection, name); err != nil {
		return fmt.Errorf("send %s scenario: %w", name, err)
	}

	var result scenarioResult
	if err := json.NewDecoder(connection).Decode(&result); err != nil {
		return fmt.Errorf("decode %s scenario result: %w", name, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode %s scenario result: %w", name, err)
	}
	fmt.Println(string(encoded))
	if result.Status != "completed" {
		return fmt.Errorf("%s scenario failed: %s", name, result.Error)
	}
	return nil
}

func listenScenarioControl() (net.Listener, error) {
	listener, err := net.Listen("unix", scenarioSocketPath)
	if err != nil && errors.Is(err, syscall.EADDRINUSE) {
		connection, dialErr := net.DialTimeout("unix", scenarioSocketPath, 500*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return nil, fmt.Errorf("scenario control socket is already in use")
		}
		if removeErr := os.Remove(scenarioSocketPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return nil, fmt.Errorf("remove stale scenario control socket: %w", removeErr)
		}
		listener, err = net.Listen("unix", scenarioSocketPath)
	}
	if err != nil {
		return nil, fmt.Errorf("listen for scenario control: %w", err)
	}
	if err := os.Chmod(scenarioSocketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(scenarioSocketPath)
		return nil, fmt.Errorf("secure scenario control socket: %w", err)
	}
	return listener, nil
}

func serveScenarioControl(listener net.Listener, logger *slog.Logger, internalKey string) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			logger.Error("scenario_control_accept_failed", "error", err)
			return
		}
		go handleScenarioControl(connection, logger, internalKey)
	}
}

func handleScenarioControl(connection net.Conn, logger *slog.Logger, internalKey string) {
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(35 * time.Second)); err != nil {
		logger.Error("scenario_control_deadline_failed", "error", err)
		return
	}
	name, err := bufio.NewReader(io.LimitReader(connection, 64)).ReadString('\n')
	if err != nil {
		logger.Error("scenario_control_read_failed", "error", err)
		return
	}
	name = strings.TrimSpace(name)
	result := executeScenario(name, logger, internalKey)
	if err := json.NewEncoder(connection).Encode(result); err != nil {
		logger.Error("scenario_control_write_failed", "scenario", name, "error", err)
	}
}

func newLogger(serviceName string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", serviceName)
}

func serve(name string, handler http.Handler, logger *slog.Logger) error {
	server := &http.Server{
		Addr:              ":" + envOrDefault("PORT", defaultPort),
		Handler:           requestLogger(logger, handler),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("server_started", "address", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	for {
		select {
		case err := <-serverErrors:
			if err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("serve %s: %w", name, err)
			}
			return nil
		case sig := <-stop:
			logger.Info("server_stopping", "signal", sig.String())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := server.Shutdown(ctx); err != nil {
				cancel()
				return fmt.Errorf("shutdown %s: %w", name, err)
			}
			cancel()
			return nil
		}
	}
}

func bootstrapConfig(ctx context.Context, application string, logger *slog.Logger) (serviceConfig, error) {
	configURL := envOrDefault("CONFIG_SERVER_URL", defaultConfigURL)
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var document configDocument
	if err := getJSON(requestCtx, &http.Client{Timeout: 10 * time.Second}, joinURL(configURL, application, "prod"), "", &document); err != nil {
		return serviceConfig{}, fmt.Errorf("bootstrap %s from config-server: %w", application, err)
	}

	config := serviceConfig{}
	for _, source := range document.PropertySources {
		if value := source.Source["routing.url"]; value != "" {
			config.RoutingURL = value
		}
		if value := source.Source["notification.url"]; value != "" {
			config.NotificationURL = value
		}
		if value := source.Source["internal.key"]; value != "" {
			config.InternalKey = value
		}
	}
	if config.InternalKey == "" {
		return serviceConfig{}, fmt.Errorf("bootstrap %s: config-server returned no internal key", application)
	}
	logger.Info("config_bootstrap", "config_server", configURL, "profile", "prod")
	return config, nil
}

func configHandler(w http.ResponseWriter, r *http.Request, internalKey string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "configuration not found"})
		return
	}

	application, profile := parts[0], parts[1]
	document := configDocument{
		Name:     application,
		Profiles: []string{profile},
		Label:    "main",
		Version:  "demo",
		PropertySources: []propertySource{{
			Name: "dispatch-config",
			Source: map[string]string{
				"routing.url":      envOrDefault("ROUTING_URL", "http://routing-api.dispatch-core.svc.cluster.local:8080"),
				"notification.url": envOrDefault("NOTIFICATION_URL", "http://notification-api.dispatch-core.svc.cluster.local:8080"),
				"internal.key":     internalKey,
			},
		}},
	}
	writeJSON(w, http.StatusOK, document)
}

func (s *service) planShipment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	ctx := r.Context()
	if err := callJSON(ctx, s.client, http.MethodPost, strings.TrimRight(s.config.RoutingURL, "/")+"/v1/routes/plan", s.config.InternalKey, map[string]string{"shipment_id": shipmentID}, nil); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "routing service unavailable"})
		return
	}
	if err := callJSON(ctx, s.client, http.MethodPost, strings.TrimRight(s.config.NotificationURL, "/")+"/v1/notifications", s.config.InternalKey, map[string]string{"shipment_id": shipmentID, "event": "planned"}, nil); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "notification service unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"shipment_id": shipmentID, "status": "planned"})
}

func (s *service) replanShipment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	const (
		shipmentPrefix = "/internal/shipments/"
		replanSuffix   = "/replan"
	)
	path := r.URL.Path
	if !strings.HasPrefix(path, shipmentPrefix) || !strings.HasSuffix(path, replanSuffix) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "shipment not found"})
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, shipmentPrefix), replanSuffix)
	if id == "" || strings.Contains(id, "/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "shipment not found"})
		return
	}

	ctx := r.Context()
	if err := callJSON(ctx, s.client, http.MethodPost, strings.TrimRight(s.config.RoutingURL, "/")+"/v1/routes/replan", s.config.InternalKey, map[string]string{"shipment_id": id}, nil); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "routing service unavailable"})
		return
	}
	if err := callJSON(ctx, s.client, http.MethodPost, strings.TrimRight(s.config.NotificationURL, "/")+"/v1/notifications", s.config.InternalKey, map[string]string{"shipment_id": id, "event": "replanned"}, nil); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "notification service unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"shipment_id": id, "status": "replanned"})
}

func (s *service) planRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"route": "route-south-01", "status": "planned"})
}

func (s *service) replanRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"route": "route-south-02", "status": "replanned"})
}

func (s *service) activeRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": []string{"route-south-01", "route-central-02"}})
}

func (s *service) createNotification(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"notification_id": "NTF-204", "status": "accepted"})
}

func (s *service) trackShipment(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/track/")
	if id == "" || id == r.URL.Path {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "shipment id is required"})
		return
	}
	positions, err := queryPositions(r.Context(), db, id)
	if err != nil {
		s.logger.Error("query_failed", "operation", "track_shipment", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "tracking unavailable"})
		return
	}
	if err := callJSON(
		r.Context(),
		s.client,
		http.MethodPost,
		strings.TrimRight(s.config.NotificationURL, "/")+"/v1/notifications",
		s.config.InternalKey,
		map[string]string{"shipment_id": id, "event": "tracking_viewed"},
		nil,
	); err != nil {
		s.logger.Error("notification_failed", "operation", "track_shipment", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "notification service unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shipment_id": id, "vehicles": positions})
}

func (s *service) fleetPositions(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	positions, err := queryPositions(r.Context(), db, "")
	if err != nil {
		s.logger.Error("query_failed", "operation", "fleet_positions", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "tracking unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vehicles": positions})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func openDatabase(ctx context.Context, logger *slog.Logger) (*sql.DB, error) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)

	pingCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		if err := db.PingContext(pingCtx); err == nil {
			logger.Info("database_ready")
			return db, nil
		}
		select {
		case <-pingCtx.Done():
			db.Close()
			return nil, fmt.Errorf("ping database: %w", pingCtx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func initializeDatabase(ctx context.Context, db *sql.DB) error {
	query := `
CREATE TABLE IF NOT EXISTS vehicle_positions (
    vehicle_id  text PRIMARY KEY,
    shipment_id text NOT NULL,
    status      text NOT NULL,
    zone        text NOT NULL,
    latitude    double precision NOT NULL,
    longitude   double precision NOT NULL
)`
	if _, err := db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("create vehicle_positions: %w", err)
	}

	seed := `
INSERT INTO vehicle_positions
    (vehicle_id, shipment_id, status, zone, latitude, longitude)
VALUES
    ('TRK-204', 'SHP-9182', 'in_transit', 'south',   -12.0464, -77.0428),
    ('TRK-331', 'SHP-7714', 'delayed',    'central', -12.0871, -77.0501),
    ('TRK-882', 'SHP-2301', 'in_transit', 'north',   -11.9345, -77.0712)
ON CONFLICT (vehicle_id) DO NOTHING`
	if _, err := db.ExecContext(ctx, seed); err != nil {
		return fmt.Errorf("seed vehicle_positions: %w", err)
	}
	return nil
}

func queryPositions(ctx context.Context, db *sql.DB, shipment string) ([]vehiclePosition, error) {
	query := `
SELECT vehicle_id,
       shipment_id,
       status,
       zone,
       latitude,
       longitude
  FROM vehicle_positions
 WHERE status IN ('in_transit', 'delayed')`
	args := []any{}
	if shipment != "" {
		query += " AND shipment_id = $1"
		args = append(args, shipment)
	}
	query += " ORDER BY vehicle_id"

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query vehicle_positions: %w", err)
	}
	defer rows.Close()

	positions := make([]vehiclePosition, 0, 3)
	for rows.Next() {
		var position vehiclePosition
		if err := rows.Scan(
			&position.VehicleID,
			&position.ShipmentID,
			&position.Status,
			&position.Zone,
			&position.Latitude,
			&position.Longitude,
		); err != nil {
			return nil, fmt.Errorf("scan vehicle_positions: %w", err)
		}
		positions = append(positions, position)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vehicle_positions: %w", err)
	}
	return positions, nil
}

func requireInternal(s *service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Key") != s.config.InternalKey {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid internal key"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		writer := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(writer, r)
		status := writer.status
		if status == 0 {
			status = http.StatusOK
		}
		logger.Info("request",
			"event", "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(started).Milliseconds(),
			"bytes", writer.bytes,
			"remote_addr", r.RemoteAddr,
		)
	})
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}

func getJSON(ctx context.Context, client *http.Client, endpoint, internalKey string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create GET request: %w", err)
	}
	if internalKey != "" {
		request.Header.Set("X-Internal-Key", internalKey)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("GET %s: status %s: %s", endpoint, response.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("decode GET %s: %w", endpoint, err)
	}
	return nil
}

func callJSON(ctx context.Context, client *http.Client, method, endpoint, internalKey string, payload, target any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, endpoint, err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("create %s request: %w", method, err)
	}
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if internalKey != "" {
		request.Header.Set("X-Internal-Key", internalKey)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("%s %s: status %s: %s", method, endpoint, response.Status, strings.TrimSpace(string(body)))
	}
	if target != nil {
		if err := json.NewDecoder(response.Body).Decode(target); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, endpoint, err)
		}
		return nil
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return nil
}

func joinURL(base string, parts ...string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return strings.TrimRight(base, "/") + "/" + strings.Join(parts, "/")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + strings.Join(parts, "/")
	return parsed.String()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
