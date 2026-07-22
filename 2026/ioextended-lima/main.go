// Package main runs a tiny HTTP workload that mimics an agent with a local command tool.
//
// This is not an agent. It has no model, planner, memory, or autonomous loop. It
// only maps prompt text to deterministic actions so the workload behavior is easy
// to reproduce during a security demo.
//
// The program intentionally uses os/exec for its actions. A pure Go version
// could read files and write paths directly, but shell execution better matches
// the kind of tool surface that makes agent workloads risky.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

const (
	listenAddr       = ":8080"
	maxRequestBytes  = 1024
	requestTimeout   = 2 * time.Second
	serviceTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	workspacePath    = "/workspace/agent-output.txt"
	rootFSPath       = "/agent-output.txt"
	clusterDNSName   = "kubernetes.default.svc.cluster.local"
)

type promptRequest struct {
	Prompt string `json:"prompt"`
}

type agentResponse struct {
	Prompt       string `json:"prompt"`
	SelectedTool string `json:"selected_tool"`
	Success      bool   `json:"success"`
	Output       string `json:"output,omitempty"`
	Error        string `json:"error,omitempty"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/run", handleRun)

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	log.Printf("agent workload listening on %s", listenAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server failed: %v", err)
	}
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	if _, err := w.Write([]byte("OK")); err != nil {
		log.Printf("write health response: %v", err)
	}
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

	var req promptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(executePrompt(r.Context(), req.Prompt)); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func executePrompt(ctx context.Context, prompt string) agentResponse {
	// The dispatch is intentionally deterministic. This PoC demonstrates workload
	// controls, not model reasoning quality.
	lowerPrompt := strings.ToLower(prompt)

	switch {
	case strings.Contains(lowerPrompt, "exec identity"), strings.Contains(lowerPrompt, "identity"):
		return runShellCommand(ctx, prompt, "check_identity", "id")
	case strings.Contains(lowerPrompt, "exec token"):
		return runShellCommand(ctx, prompt, "exec_read_service_account_token", "head -c 10 "+serviceTokenPath)
	case strings.Contains(lowerPrompt, "token"):
		return runShellCommand(ctx, prompt, "read_service_account_token", "head -c 10 "+serviceTokenPath+" && printf ...")
	case strings.Contains(lowerPrompt, "exec rootfs"), strings.Contains(lowerPrompt, "rootfs"):
		return runShellCommand(ctx, prompt, "write_rootfs_file", "printf data > "+rootFSPath+" && echo wrote "+rootFSPath)
	case strings.Contains(lowerPrompt, "exec workspace"), strings.Contains(lowerPrompt, "workspace"):
		return runShellCommand(ctx, prompt, "write_workspace_file", "printf data > "+workspacePath+" && echo wrote "+workspacePath)
	case strings.Contains(lowerPrompt, "metadata"):
		return runShellCommand(ctx, prompt, "call_metadata_server", "wget -T 2 -O /dev/null http://gnu.org")
	case strings.Contains(lowerPrompt, "external"):
		return runShellCommand(ctx, prompt, "call_external_api", "wget -T 2 -O /dev/null https://example.com")
	case strings.Contains(lowerPrompt, "dns"):
		return runShellCommand(ctx, prompt, "resolve_dns", "nslookup "+clusterDNSName)
	case strings.Contains(lowerPrompt, "environment"):
		return runShellCommand(ctx, prompt, "show_environment", "env")
	default:
		return fail(prompt, "unknown", "No tool configured for this prompt.")
	}
}

func ok(prompt, tool, output string) agentResponse {
	return agentResponse{Prompt: prompt, SelectedTool: tool, Success: true, Output: output}
}

func fail(prompt, tool, errText string) agentResponse {
	return agentResponse{Prompt: prompt, SelectedTool: tool, Success: false, Error: errText}
}

func runShellCommand(ctx context.Context, prompt, tool, command string) agentResponse {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	output, err := exec.CommandContext(ctx, "/bin/sh", "-c", command).CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		return fail(prompt, tool, strings.TrimSpace(text+"\n"+err.Error()))
	}
	return ok(prompt, tool, text)
}
