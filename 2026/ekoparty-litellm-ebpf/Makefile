SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help
.DELETE_ON_ERROR:
.ONESHELL:
.NOTPARALLEL: up ready stop

PROJECT_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))

KUBECTL ?= kubectl
KUBECTL_FLAGS ?=
HELM ?= helm
HELM_FLAGS ?=
CURL ?= curl
CURL_FLAGS ?=
SEQ ?= seq

NAMESPACE ?= ai
TETRAGON_NAMESPACE ?= kube-system
TETRAGON_VERSION ?= 1.7.1
ROLLOUT_TIMEOUT ?= 30m
HELM_TIMEOUT ?= 15m
MODEL ?= qwen2.5:0.5b-instruct
UI_LOCAL_PORT ?= 8080
GATEWAY_LOCAL_PORT ?= 4000
RUNTIME_DIR ?= /tmp/ekoparty-litellm-ebpf

AI_MANIFEST := $(PROJECT_DIR)/llm-chat-stack.yaml

.PHONY: help up ready status stop

help: ## Show the available commands.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ {print "  " $$1 "  " $$2}' $(MAKEFILE_LIST)

up: ## Install Tetragon, deploy the stack, pull the model, and open local ports.
	for command in $(KUBECTL) $(HELM) $(CURL) $(SEQ) nohup; do
		command -v "$$command" >/dev/null || { echo "Required command not found: $$command" >&2; exit 1; }
	done
	$(KUBECTL) $(KUBECTL_FLAGS) config current-context
	$(HELM) $(HELM_FLAGS) repo add cilium https://helm.cilium.io --force-update
	$(HELM) $(HELM_FLAGS) repo update cilium
	$(HELM) $(HELM_FLAGS) upgrade --install tetragon cilium/tetragon \
		--version $(TETRAGON_VERSION) \
		--set tetragon.extraArgs.parents-map-enabled=true \
		--namespace $(TETRAGON_NAMESPACE) \
		--wait --timeout=$(HELM_TIMEOUT)
	$(KUBECTL) $(KUBECTL_FLAGS) -n $(TETRAGON_NAMESPACE) rollout status daemonset/tetragon --timeout=$(ROLLOUT_TIMEOUT)
	$(KUBECTL) $(KUBECTL_FLAGS) create namespace $(NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) $(KUBECTL_FLAGS) apply -f -
	$(KUBECTL) $(KUBECTL_FLAGS) apply -f $(AI_MANIFEST)
	$(MAKE) ready
	$(KUBECTL) $(KUBECTL_FLAGS) exec -n $(NAMESPACE) deployment/llm -- ollama pull $(MODEL)
	$(KUBECTL) $(KUBECTL_FLAGS) exec -n $(NAMESPACE) deployment/llm -- ollama list
	mkdir -p $(RUNTIME_DIR)
	start_forward() {
		name="$$1"; local_port="$$2"; remote_port="$$3"; destination="$$4"; health_url="$$5"
		pid_file="$(RUNTIME_DIR)/$${name}.pid"
		log_file="$(RUNTIME_DIR)/$${name}.log"
		if [ -s "$$pid_file" ]; then
			pid=$$(cat "$$pid_file")
			if kill -0 "$$pid" 2>/dev/null; then
				for attempt in $$($(SEQ) 1 30); do
					if $(CURL) $(CURL_FLAGS) --fail --silent --max-time 2 "$$health_url" >/dev/null; then
						echo "$$name forward is ready on 127.0.0.1:$$local_port"
						return 0
					fi
					if ! kill -0 "$$pid" 2>/dev/null; then break; fi
					sleep 1
				done
				if kill -0 "$$pid" 2>/dev/null; then
					echo "Existing $$name forward is not healthy; see $$log_file" >&2
					cat "$$log_file" >&2 || true
					return 1
				fi
			fi
			rm -f "$$pid_file"
		fi
		nohup $(KUBECTL) $(KUBECTL_FLAGS) -n $(NAMESPACE) port-forward --address=127.0.0.1 "$$destination" "$$local_port:$$remote_port" >"$$log_file" 2>&1 </dev/null &
		pid=$$!
		echo "$$pid" >"$$pid_file"
		for attempt in $$($(SEQ) 1 30); do
			if $(CURL) $(CURL_FLAGS) --fail --silent --max-time 2 "$$health_url" >/dev/null; then
				echo "$$name forward is ready on 127.0.0.1:$$local_port"
				return 0
			fi
			if ! kill -0 "$$pid" 2>/dev/null; then
				echo "Could not start $$name forward; see $$log_file" >&2
				cat "$$log_file" >&2 || true
				return 1
			fi
			sleep 1
		done
		echo "Timed out waiting for $$name forward; see $$log_file" >&2
		cat "$$log_file" >&2 || true
		return 1
	}
	start_forward ui $(UI_LOCAL_PORT) 8080 deployment/ui http://127.0.0.1:$(UI_LOCAL_PORT)/health
	start_forward gateway $(GATEWAY_LOCAL_PORT) 4000 service/gw http://127.0.0.1:$(GATEWAY_LOCAL_PORT)/health/liveliness
	@echo
	@echo 'Ekoparty lab is ready.'
	@echo 'Open WebUI: http://127.0.0.1:$(UI_LOCAL_PORT)'
	@echo 'LiteLLM:    http://127.0.0.1:$(GATEWAY_LOCAL_PORT)'
	@echo 'Next: load the audit policy and follow README.md.'

ready: ## Wait for Tetragon, Open WebUI, LiteLLM, and Ollama.
	$(KUBECTL) $(KUBECTL_FLAGS) -n $(TETRAGON_NAMESPACE) rollout status daemonset/tetragon --timeout=$(ROLLOUT_TIMEOUT)
	for deployment in ui gw llm; do
		$(KUBECTL) $(KUBECTL_FLAGS) -n $(NAMESPACE) wait --for=condition=Available deployment/"$$deployment" --timeout=$(ROLLOUT_TIMEOUT)
	done

status: ## Show lab workloads, storage, and the Tetragon policy.
	$(KUBECTL) $(KUBECTL_FLAGS) -n $(TETRAGON_NAMESPACE) get daemonset/tetragon
	$(KUBECTL) $(KUBECTL_FLAGS) -n $(NAMESPACE) get deployments,pods,services,pvc,configmap,secret
	$(KUBECTL) $(KUBECTL_FLAGS) -n $(NAMESPACE) get tracingpoliciesnamespaced

stop: ## Stop local port-forwards; leave Kubernetes workloads running.
	for name in ui gateway; do
		pid_file='$(RUNTIME_DIR)/'"$$name"'.pid'
		if [ -s "$$pid_file" ]; then
			pid=$$(cat "$$pid_file")
			if kill -0 "$$pid" 2>/dev/null; then kill "$$pid"; fi
			rm -f "$$pid_file"
		fi
	done

