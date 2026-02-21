.PHONY: up down logs clean \
        test-add test-switch test-remove test-debug \
        test-split-horizon list

# ── Stack Management ──────────────────────────────────────────────────────────

up:
	docker compose up --build -d
	@echo ""
	@echo "Stack is up. Endpoints:"
	@echo "  Management API  : http://localhost:8080"
	@echo "  VPS Envoy       : http://localhost:10000   ← simulates internet-facing edge"
	@echo "  Home Envoy      : http://localhost:10001   ← simulates homeserver Envoy (debug)"
	@echo "  Home Envoy admin: http://localhost:9901"
	@echo "  VPS Envoy admin : http://localhost:9902"
	@echo ""
	@echo "Run 'make test-add' to register a service, then"
	@echo "    'make test-split-horizon' to verify both routing paths."

down:
	docker compose down

logs:
	docker compose logs -f

clean: down
	docker compose rm -f

# ── Tracer Bullet Tests ───────────────────────────────────────────────────────

# Step 1: Register upstream A and verify routing works through the full stack.
test-add:
	@echo ">>> Adding service 'web' → web-a:5678"
	curl -s -X POST http://localhost:8080/services \
		-H 'Content-Type: application/json' \
		-d '{"name":"web","domain":"web.example.com","upstream":"web-a:5678"}'
	@echo ""
	@sleep 2
	@echo ">>> VPS Envoy (port 10000) — should show 'Hello from upstream A':"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000
	@echo ""

# Step 2: Verify the Split-Horizon routing explicitly.
# VPS Envoy (port 10000) → Home Envoy (port 10001) → web-a:5678
# Both paths should return "Hello from upstream A".
test-split-horizon:
	@echo ">>> Split-Horizon verification"
	@echo ""
	@echo "--- Path 1: VPS Envoy (port 10000) → Home Envoy → web-a ---"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000
	@echo ""
	@echo "--- Path 2: Home Envoy direct (port 10001) → web-a ---"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10001
	@echo ""
	@echo "Both should return 'Hello from upstream A'."
	@echo "If Path 1 works and Path 2 works, the Split-Horizon routing is correct."

# Step 3: Switch upstream to web-b. Both paths should update without restart.
test-switch:
	@echo ">>> Removing service 'web'"
	curl -s -X DELETE http://localhost:8080/services/web
	@echo ""
	@echo ">>> Adding service 'web' → web-b:5678"
	curl -s -X POST http://localhost:8080/services \
		-H 'Content-Type: application/json' \
		-d '{"name":"web","domain":"web.example.com","upstream":"web-b:5678"}'
	@echo ""
	@sleep 2
	@echo ">>> VPS Envoy (should show 'Hello from upstream B'):"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000
	@echo ""

# Step 4: Remove service. Both Envoys should return 404.
test-remove:
	@echo ">>> Removing service 'web'"
	curl -s -X DELETE http://localhost:8080/services/web
	@echo ""
	@sleep 2
	@echo ">>> VPS Envoy (should get 404):"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000
	@echo ""
	@echo ">>> Home Envoy (should get 404):"
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10001
	@echo ""

# Verbose output for debugging the full HTTP exchange.
test-debug:
	@echo ">>> Adding service 'web' → web-a:5678"
	curl -s -X POST http://localhost:8080/services \
		-H 'Content-Type: application/json' \
		-d '{"name":"web","domain":"web.example.com","upstream":"web-a:5678"}'
	@echo ""
	@sleep 2
	@echo ">>> Verbose request via VPS Envoy:"
	curl -v -H "Host: web.example.com" http://localhost:10000 2>&1
	@echo ""

# Show current service registry state.
list:
	curl -s http://localhost:8080/services | python3 -m json.tool 2>/dev/null || \
	curl -s http://localhost:8080/services
