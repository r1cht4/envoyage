.PHONY: up down logs clean \
        test-auto test-manual-b test-split-horizon list \
        wg-up wg-down wg-status

# ── Stack Management ──────────────────────────────────────────────────────────

up:
	docker compose up --build -d
	@echo ""
	@echo "Stack is up (Home node). Endpoints:"
	@echo "  Management API  : http://localhost:8080"
	@echo "  Home Envoy      : http://localhost:10000"
	@echo "  Home Envoy admin: http://localhost:9901"
	@echo "  xDS gRPC        : :9090 (reachable from VPS via WireGuard)"
	@echo ""
	@echo "VPS Envoy runs separately: docker compose -f docker-compose.vps.yml up -d"

down:
	docker compose down

logs:
	docker compose logs -f

clean: down
	docker compose rm -f

# ── WireGuard helpers ─────────────────────────────────────────────────────────

wg-up:
	sudo wg-quick up wg0

wg-down:
	sudo wg-quick down wg0

wg-status:
	sudo wg show

# ── Tests ─────────────────────────────────────────────────────────────────────

test-auto:
	@echo ">>> Waiting for Docker watcher to discover web-a..."
	@sleep 3
	@echo ""
	@echo ">>> Registry (web-a should appear automatically):"
	curl -s http://localhost:8080/services | python3 -m json.tool
	@echo ""
	@echo ">>> Home Envoy — should return 'Hello from upstream A':"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000

test-e2e:
	@echo ">>> End-to-end test via VPS public IP:"
	@echo "    Path: Internet → VPS Envoy → WireGuard → Home Envoy → web-a"
	curl -s -w "\n[HTTP %{http_code}]\n" \
		-H "Host: web.example.com" \
		http://${ENVOYAGE_VPS_PUBLIC_IP}:10000

test-split-horizon:
	@echo ">>> Home Envoy (local):"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000
	@echo ""
	@echo ">>> VPS Envoy (via WireGuard IP, if tunnel is up):"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" \
		http://${ENVOYAGE_VPS_PUBLIC_IP:-185.244.195.40}:10000

list:
	curl -s http://localhost:8080/services | python3 -m json.tool 2>/dev/null || \
	curl -s http://localhost:8080/services