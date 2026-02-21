.PHONY: up down logs clean \
        test-auto test-manual-b test-split-horizon \
        test-add test-switch test-remove test-debug list

# ── Stack Management ──────────────────────────────────────────────────────────

up:
	docker compose up --build -d
	@echo ""
	@echo "Stack is up. Endpoints:"
	@echo "  Management API  : http://localhost:8080"
	@echo "  VPS Envoy       : http://localhost:10000   ← internet-facing edge"
	@echo "  Home Envoy      : http://localhost:10001   ← homeserver (debug)"
	@echo "  Home Envoy admin: http://localhost:9901"
	@echo "  VPS Envoy admin : http://localhost:9902"
	@echo ""
	@echo "web-a is auto-discovered via Docker labels."
	@echo "Run 'make test-auto' to verify, or 'make logs' to watch the watcher."

down:
	docker compose down

logs:
	docker compose logs -f

clean: down
	docker compose rm -f

# ── Docker Watcher Tests ──────────────────────────────────────────────────────

# Primary watcher test: web-a has envoyage labels in docker-compose.yml,
# so it should be auto-discovered without any manual API call.
# Wait a few seconds for the watcher's initial sync to complete.
test-auto:
	@echo ">>> Waiting for Docker watcher to discover web-a..."
	@sleep 3
	@echo ""
	@echo ">>> Checking registry (web-a should appear automatically):"
	curl -s http://localhost:8080/services | python3 -m json.tool
	@echo ""
	@echo ">>> VPS Envoy — should return 'Hello from upstream A':"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000
	@echo ""
	@echo ">>> Home Envoy — should also return 'Hello from upstream A':"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10001

# Register web-b manually (no labels on web-b in docker-compose.yml).
# This proves the management API still works alongside the watcher.
test-manual-b:
	@echo ">>> Manually registering web-b via API..."
	curl -s -X POST http://localhost:8080/services \
		-H 'Content-Type: application/json' \
		-d '{"name":"web-b","domain":"web-b.example.com","upstream":"web-b:5678"}'
	@echo ""
	@sleep 1
	@echo ">>> VPS Envoy — should return 'Hello from upstream B':"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web-b.example.com" http://localhost:10000

# Verify the Split-Horizon routing still works correctly alongside the watcher.
test-split-horizon:
	@echo ">>> Split-Horizon verification (both paths → web-a)"
	@echo ""
	@echo "--- Path 1: VPS Envoy → Home Envoy → web-a ---"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000
	@echo ""
	@echo "--- Path 2: Home Envoy direct → web-a ---"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10001

# ── Legacy Manual Tests (still work) ─────────────────────────────────────────
# These use the management API directly, bypassing the watcher.
# Useful for testing services without Docker labels.

test-add:
	@echo ">>> Adding service 'web' → web-a:5678 via API"
	curl -s -X POST http://localhost:8080/services \
		-H 'Content-Type: application/json' \
		-d '{"name":"web","domain":"web.example.com","upstream":"web-a:5678"}'
	@echo ""
	@sleep 2
	@echo ">>> VPS Envoy:"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000

test-switch:
	@echo ">>> Removing 'web', re-adding → web-b:5678"
	curl -s -X DELETE http://localhost:8080/services/web
	@echo ""
	curl -s -X POST http://localhost:8080/services \
		-H 'Content-Type: application/json' \
		-d '{"name":"web","domain":"web.example.com","upstream":"web-b:5678"}'
	@echo ""
	@sleep 2
	@echo ">>> VPS Envoy (should show 'Hello from upstream B'):"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000

test-remove:
	@echo ">>> Removing 'web'"
	curl -s -X DELETE http://localhost:8080/services/web
	@echo ""
	@sleep 2
	@echo ">>> VPS Envoy (should get 404):"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000
	@echo ""
	@echo ">>> Home Envoy (should get 404):"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10001

test-debug:
	@echo ">>> Adding 'web' → web-a:5678"
	curl -s -X POST http://localhost:8080/services \
		-H 'Content-Type: application/json' \
		-d '{"name":"web","domain":"web.example.com","upstream":"web-a:5678"}'
	@echo ""
	@sleep 2
	@echo ">>> Verbose request via VPS Envoy:"
	curl -v -H "Host: web.example.com" http://localhost:10000 2>&1

list:
	curl -s http://localhost:8080/services | python3 -m json.tool 2>/dev/null || \
	curl -s http://localhost:8080/services