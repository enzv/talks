SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help
.DELETE_ON_ERROR:
.ONESHELL:
.NOTPARALLEL: up ready reset down stop start

PROJECT_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
CACHE_DIR ?= /tmp/fleet-commander

DOCKER ?= docker
GIT ?= git
HELM ?= helm
KUBECTL ?= kubectl
MINIKUBE ?= minikube
CURL ?= curl
SED ?= sed

CONTROL_PLANE ?= control-plane
WORKER_A ?= worker-a
WORKER_B ?= worker-b
NETWORK ?= fleet-local
NETWORK_SUBNET ?= 172.28.0.0/16
NODE_PREFIX ?= 172.28.0
LB_PREFIX ?= 172.28

KUBERNETES_VERSION ?= v1.34.0
KIC_IMAGE ?= gcr.io/k8s-minikube/kicbase:v0.0.51@sha256:4a1c825b61479e6c898851ea66f13c620aaeab6002746e95067fc2c4b38a0b24
MINIKUBE_VERSION ?= v1.39.0
CROSSPLANE_VERSION ?= 2.4.0
MODELPLANE_VERSION ?= v0.4.0
METALLB_VERSION ?= v0.14.8
ENVOY_GATEWAY_VERSION ?= v1.8.1

MODELPLANE_DIR := $(CACHE_DIR)/modelplane-$(MODELPLANE_VERSION)
DRA_DIR := $(CACHE_DIR)/dra-example-driver-$(MODELPLANE_VERSION)
DRA_IMAGE ?= fleet/dra-example-driver:$(MODELPLANE_VERSION)-141gi

MODELPLANE_URL := https://github.com/modelplaneai/modelplane.git
DRA_URL := https://github.com/kubernetes-sigs/dra-example-driver.git

.PHONY: help up ready reset down stop start \
	_check _network _clusters _metallb _gateway-api _sources _dra _control-plane _fleet

help: ## Show the available commands.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  %-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

up: _check _network _clusters _metallb _gateway-api _sources _dra _control-plane _fleet ready ## Create and prepare the complete local demo.
	@echo
	@echo "Fleet Commander is ready."
	@echo "Apply the workload during the talk with:"
	@echo "  $(KUBECTL) --context $(CONTROL_PLANE) apply -f conference-modeldeployment.yaml"

ready: _check ## Verify the local fleet and leave the workload absent.
	@echo "Checking local Modelplane demo..."
	for context in $(CONTROL_PLANE) $(WORKER_A) $(WORKER_B); do
		$(KUBECTL) --context "$$context" cluster-info >/dev/null
	done
	$(KUBECTL) --context $(CONTROL_PLANE) -n crossplane-system \
		rollout status deployment/crossplane --timeout=30s
	$(KUBECTL) --context $(CONTROL_PLANE) \
		wait --for=condition=Healthy configuration.pkg.crossplane.io/modelplane --timeout=30s
	for cluster in $(WORKER_A) $(WORKER_B); do
		ready_status="$$($(KUBECTL) --context $(CONTROL_PLANE) get inferencecluster "$$cluster" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
		memory="$$($(KUBECTL) --context $(CONTROL_PLANE) get inferencecluster "$$cluster" -o jsonpath='{.status.gpuPools[0].devices[0].capacity.memory.value}')"
		test "$$ready_status" = True
		test -n "$$memory"
		printf '  %-12s Ready, %s\n' "$$cluster" "$$memory"
		$(KUBECTL) --context "$$cluster" -n dra-example-driver \
			rollout status daemonset/dra-example-driver-kubeletplugin --timeout=30s
		test "$$($(KUBECTL) --context "$$cluster" get resourceslices --no-headers 2>/dev/null | wc -l)" -gt 0
		test "$$($(KUBECTL) --context "$$cluster" get deviceclasses --no-headers 2>/dev/null | wc -l)" -gt 0
	done
	if $(KUBECTL) --context $(CONTROL_PLANE) -n ml-team get modeldeployment conference-model >/dev/null 2>&1; then
		echo "conference-model still exists; run 'make reset' first" >&2
		exit 1
	fi
	if $(KUBECTL) --context $(CONTROL_PLANE) -n ml-team get modelreplicas -l modelplane.ai/deployment=conference-model --no-headers 2>/dev/null | grep -q .; then
		echo "conference-model ModelReplica still exists; run 'make reset' first" >&2
		exit 1
	fi
	echo "Ready."

reset: _check ## Remove the conference workload and its worker-side objects.
	@echo "Removing conference-model..."
	$(KUBECTL) --context $(CONTROL_PLANE) -n ml-team \
		delete modeldeployment conference-model --ignore-not-found --wait=false

	deadline=$$(( $$(date +%s) + 180 ))
	while test $$(date +%s) -lt $$deadline; do
		remaining="$$($(KUBECTL) --context $(CONTROL_PLANE) -n ml-team get modelreplicas \
			-l modelplane.ai/deployment=conference-model -o name 2>/dev/null || true)"
		test -z "$$remaining" && break
		sleep 2
	done
	if test -n "$$remaining"; then
		echo "ModelReplica cleanup timed out." >&2
		exit 1
	fi

	for context in $(WORKER_A) $(WORKER_B); do
		pods="$$($(KUBECTL) --context "$$context" -n default get pods \
			-l modelplane.ai/serving -o go-template='{{range .items}}{{.metadata.name}}{{"\t"}}{{index .metadata.labels "modelplane.ai/serving"}}{{"\n"}}{{end}}' 2>/dev/null || true)"
		while IFS=$$'\t' read -r pod serving; do
			test -n "$$pod" || continue
			case "$$serving" in conference-model-*)
				$(KUBECTL) --context "$$context" -n default delete pod "$$pod" \
					--ignore-not-found --wait=false --grace-period=15 || true
			;; esac
		done <<< "$$pods"
		done

	deadline=$$(( $$(date +%s) + 180 ))
	force_after=$$(( $$(date +%s) + 25 ))
	while test $$(date +%s) -lt $$deadline; do
		found=false
		for context in $(WORKER_A) $(WORKER_B); do
			pod_rows="$$($(KUBECTL) --context "$$context" -n default get pods \
				-l modelplane.ai/serving -o go-template='{{range .items}}{{.metadata.name}}{{"\t"}}{{index .metadata.labels "modelplane.ai/serving"}}{{"\n"}}{{end}}' 2>/dev/null || true)"
			while IFS=$$'\t' read -r pod serving; do
				test -n "$$pod" || continue
				case "$$serving" in conference-model-*)
					found=true
					if test $$(date +%s) -ge $$force_after; then
						$(KUBECTL) --context "$$context" -n default delete pod "$$pod" \
							--ignore-not-found --wait=false --grace-period=0 --force || true
					fi
				;; esac
			done <<< "$$pod_rows"
			claims="$$($(KUBECTL) --context "$$context" -n default get resourceclaims -o name 2>/dev/null | grep 'conference-model-' || true)"
			if test -n "$$claims"; then found=true; fi
			if test -n "$$claims" && test $$(date +%s) -ge $$force_after; then
				for claim in $$claims; do
					$(KUBECTL) --context "$$context" -n default delete "$$claim" \
						--ignore-not-found --wait=false || true
				done
			fi
		done
		if ! $$found; then
			echo "Reset complete."
			exit 0
		fi
		sleep 2
	done
	echo "Reset timed out; inspect the worker Pods and ResourceClaims." >&2
	exit 1

down: _check ## Delete the three Minikube profiles and the shared Docker network.
	for profile in $(CONTROL_PLANE) $(WORKER_A) $(WORKER_B); do
		$(MINIKUBE) delete -p "$$profile" || true
	done
	$(DOCKER) network rm $(NETWORK) >/dev/null 2>&1 || true

stop: _check ## Stop the three Minikube profiles and keep their data.
	for profile in $(CONTROL_PLANE) $(WORKER_A) $(WORKER_B); do
		$(MINIKUBE) stop -p "$$profile"
	done

start: _check ## Start existing Minikube profiles without recreating them.
	for profile in $(CONTROL_PLANE) $(WORKER_A) $(WORKER_B); do
		$(MINIKUBE) start -p "$$profile"
	done

_check:
	for command in $(DOCKER) $(GIT) $(HELM) $(KUBECTL) $(MINIKUBE) $(CURL) $(SED); do
		command -v "$$command" >/dev/null || { echo "Required command not found: $$command" >&2; exit 1; }
	done
	$(DOCKER) info >/dev/null
	minikube_version="$$($(MINIKUBE) version --short)"
	case "$$minikube_version" in
		$(MINIKUBE_VERSION)*) ;;
		*)
			echo "Minikube $(MINIKUBE_VERSION) is required; found $$minikube_version." >&2
			echo "Use MINIKUBE=/path/to/minikube-$(MINIKUBE_VERSION), then run make up again." >&2
			exit 1
		;;
	esac

_network:
	$(DOCKER) network inspect $(NETWORK) >/dev/null 2>&1 || \
		$(DOCKER) network create --subnet $(NETWORK_SUBNET) $(NETWORK)

_clusters:
	for profile_ip in \
		"$(CONTROL_PLANE):$(NODE_PREFIX).10:4:8192" \
		"$(WORKER_A):$(NODE_PREFIX).11:2:6144" \
		"$(WORKER_B):$(NODE_PREFIX).12:2:6144"; do
		IFS=: read -r profile ip cpus memory <<< "$$profile_ip"
		$(MINIKUBE) start -p "$$profile" --driver=docker --network=$(NETWORK) \
			--container-runtime=containerd --kubernetes-version=$(KUBERNETES_VERSION) \
			--base-image=$(KIC_IMAGE) --static-ip="$$ip" --apiserver-ips="$$ip" \
			--cpus="$$cpus" --memory="$$memory" --wait=all --wait-timeout=15m
		# Three local control planes can exceed the default host inotify limit.
		$(DOCKER) exec "$$profile" sysctl -w fs.inotify.max_user_instances=1024 >/dev/null 2>&1 || true
	done

_metallb:
	for context in $(WORKER_A) $(WORKER_B); do
		$(KUBECTL) --context "$$context" apply -f \
			https://raw.githubusercontent.com/metallb/metallb/$(METALLB_VERSION)/config/manifests/metallb-native.yaml
		$(KUBECTL) --context "$$context" -n metallb-system \
			rollout status deployment/controller --timeout=5m
	done
	for context in $(WORKER_A) $(WORKER_B); do
		if test "$$context" = $(WORKER_A); then start=100; end=149; else start=150; end=199; fi
		$(KUBECTL) --context "$$context" apply -f <(printf '%s\n' \
			'apiVersion: metallb.io/v1beta1' \
			'kind: IPAddressPool' \
			'metadata:' \
			'  name: fleet-local' \
			'  namespace: metallb-system' \
			'spec:' \
			'  addresses: ["$(LB_PREFIX).255.'"$$start"'-$(LB_PREFIX).255.'"$$end"'"]' \
			'---' \
			'apiVersion: metallb.io/v1beta1' \
			'kind: L2Advertisement' \
			'metadata:' \
			'  name: fleet-local' \
			'  namespace: metallb-system' \
			'spec:' \
			'  ipAddressPools: [fleet-local]')
	done

_gateway-api:
	for context in $(WORKER_A) $(WORKER_B); do
		$(KUBECTL) --context "$$context" apply --server-side \
			--field-manager=fleet-bootstrap \
			-f https://raw.githubusercontent.com/envoyproxy/gateway/$(ENVOY_GATEWAY_VERSION)/charts/gateway-helm/charts/crds/crds/gatewayapi-crds.yaml
	done

_sources: | $(CACHE_DIR)
	if test ! -f $(MODELPLANE_DIR)/e2e/lean-control-plane.yaml; then
		$(GIT) clone --depth 1 --branch $(MODELPLANE_VERSION) $(MODELPLANE_URL) $(MODELPLANE_DIR)
	fi
	if test ! -f $(DRA_DIR)/internal/profiles/gpu/gpu.go; then
		$(GIT) clone --depth 1 --branch $(MODELPLANE_VERSION) $(DRA_URL) $(DRA_DIR)
	fi

_dra: _sources
	for context in $(WORKER_A) $(WORKER_B); do
		$(KUBECTL) --context "$$context" apply -f $(MODELPLANE_DIR)/e2e/dra-example-driver.yaml
		node="$$($(KUBECTL) --context "$$context" get nodes -o jsonpath='{.items[0].metadata.name}')"
		$(KUBECTL) --context "$$context" label node "$$node" \
			modelplane.ai/pool=gpu-synthetic --overwrite
	done
	$(SED) -i 's/resource.MustParse("80Gi")/resource.MustParse("141Gi")/' \
		$(DRA_DIR)/internal/profiles/gpu/gpu.go
	if ! $(DOCKER) image inspect $(DRA_IMAGE) >/dev/null 2>&1; then
		$(DOCKER) build --build-arg GO_VERSION=1.26.2 \
			--build-arg BASE_IMAGE=docker.io/ubuntu:22.04 \
			--build-arg VERSION=$(MODELPLANE_VERSION)-141gi \
			-f $(DRA_DIR)/deployments/container/Dockerfile -t $(DRA_IMAGE) $(DRA_DIR)
	else
		echo "Using existing image $(DRA_IMAGE)."
	fi
	$(MINIKUBE) -p $(WORKER_B) image load --daemon $(DRA_IMAGE)
	$(KUBECTL) --context $(WORKER_B) -n dra-example-driver set image \
		daemonset/dra-example-driver-kubeletplugin plugin=$(DRA_IMAGE)
	for context in $(WORKER_A) $(WORKER_B); do
		$(KUBECTL) --context "$$context" -n dra-example-driver \
			rollout status daemonset/dra-example-driver-kubeletplugin --timeout=5m
	done

_control-plane: _sources
	$(HELM) repo add crossplane-stable https://charts.crossplane.io/stable >/dev/null 2>&1 || true
	$(HELM) --kube-context $(CONTROL_PLANE) upgrade --install crossplane crossplane-stable/crossplane \
		--version $(CROSSPLANE_VERSION) --namespace crossplane-system --create-namespace
	$(KUBECTL) --context $(CONTROL_PLANE) -n crossplane-system \
		rollout status deployment/crossplane --timeout=5m
	$(KUBECTL) --context $(CONTROL_PLANE) apply -f $(MODELPLANE_DIR)/e2e/lean-control-plane.yaml
	$(KUBECTL) --context $(CONTROL_PLANE) apply -f \
		https://raw.githubusercontent.com/modelplaneai/modelplane/$(MODELPLANE_VERSION)/docs/manifests/getting-started/prerequisites.yaml
	$(KUBECTL) --context $(CONTROL_PLANE) apply -f <(printf '%s\n' \
		'apiVersion: pkg.crossplane.io/v1' \
		'kind: Configuration' \
		'metadata:' \
		'  name: modelplane' \
		'spec:' \
		'  package: xpkg.upbound.io/modelplane/modelplane:$(MODELPLANE_VERSION)')
	$(KUBECTL) --context $(CONTROL_PLANE) wait --for=condition=Healthy \
		configuration.pkg.crossplane.io/modelplane --timeout=25m
	$(KUBECTL) --context $(CONTROL_PLANE) patch provider.pkg.crossplane.io upbound-provider-helm \
		--type=merge -p '{"spec":{"runtimeConfigRef":{"apiVersion":"pkg.crossplane.io/v1beta1","kind":"DeploymentRuntimeConfig","name":"provider-helm-modelplane"}}}'

_fleet:
	$(KUBECTL) --context $(CONTROL_PLANE) -n modelplane-system create secret generic local-cluster-kubeconfig \
		--from-file=kubeconfig=<( $(KUBECTL) config view --raw --flatten --minify --context $(WORKER_A) ) \
		--dry-run=client -o yaml | $(KUBECTL) --context $(CONTROL_PLANE) apply -f -
	$(KUBECTL) --context $(CONTROL_PLANE) -n modelplane-system create secret generic worker-b-cluster-kubeconfig \
		--from-file=kubeconfig=<( $(KUBECTL) config view --raw --flatten --minify --context $(WORKER_B) ) \
		--dry-run=client -o yaml | $(KUBECTL) --context $(CONTROL_PLANE) apply -f -
	$(KUBECTL) --context $(CONTROL_PLANE) create namespace ml-team --dry-run=client -o yaml | \
		$(KUBECTL) --context $(CONTROL_PLANE) apply -f -
	$(SED) 's/172\\.18/$(LB_PREFIX)/g' $(PROJECT_DIR)/platform.yaml | \
		$(KUBECTL) --context $(CONTROL_PLANE) apply -f -
	$(KUBECTL) --context $(CONTROL_PLANE) wait --for=condition=Ready \
		inferencecluster/$(WORKER_A) inferencecluster/$(WORKER_B) --timeout=25m

$(CACHE_DIR):
	mkdir -p $@
