.PHONY: up down test-add test-switch test-remove logs clean

# Start the full stack: control plane + envoy + dummy upstreams
up:
	docker compose up --build -d
	@echo ""
	@echo "Stack is up. Endpoints:"
	@echo "  Management API:  http://localhost:8080"
	@echo "  Envoy data plane: http://localhost:10000"
	@echo "  Envoy admin:     http://localhost:9901"
	@echo ""
	@echo "Run 'make test-add' to register a service."

down:
	docker compose down

logs:
	docker compose logs -f

# --- Tracer bullet tests ---

# Step 1: Register upstream A. After this, requests to web.example.com
# via Envoy should hit web-a and return "Hello from upstream A".
test-add:
	@echo ">>> Adding service 'web' → web-a:5678"
	curl -s -X POST http://localhost:8080/services \
		-H 'Content-Type: application/json' \
		-d '{"name":"web","domain":"web.example.com","upstream":"web-a:5678"}'
	@echo ""
	@echo ">>> Testing routing (should show 'Hello from upstream A'):"
	@sleep 2
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000
	@echo ""

# Step 2: Remove service 'web' and re-add it pointing to web-b.
# This proves dynamic config updates work without Envoy restart.
test-switch:
	@echo ">>> Removing service 'web'"
	curl -s -X DELETE http://localhost:8080/services/web
	@echo ""
	@echo ">>> Adding service 'web' → web-b:5678"
	curl -s -X POST http://localhost:8080/services \
		-H 'Content-Type: application/json' \
		-d '{"name":"web","domain":"web.example.com","upstream":"web-b:5678"}'
	@echo ""
	@echo ">>> Testing routing (should show 'Hello from upstream B'):"
	@sleep 2
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000
	@echo ""

# Step 3: Remove the service. Requests should now get 404 (no matching route).
test-remove:
	@echo ">>> Removing service 'web'"
	curl -s -X DELETE http://localhost:8080/services/web
	@echo ""
	@echo ">>> Testing routing (should get 404):"
	@sleep 2
	curl -s -w "\n[HTTP %{http_code}]\n" -H "Host: web.example.com" http://localhost:10000

# Verbose test for debugging — shows full HTTP exchange
test-debug:
	@echo ">>> Adding service 'web' → web-a:5678"
	curl -s -X POST http://localhost:8080/services \
		-H 'Content-Type: application/json' \
		-d '{"name":"web","domain":"web.example.com","upstream":"web-a:5678"}'
	@echo ""
	@sleep 2
	@echo ">>> Verbose request to Envoy:"
	curl -v -H "Host: web.example.com" http://localhost:10000 2>&1
	@echo ""

# List current services
list:
	curl -s http://localhost:8080/services | python3 -m json.tool 2>/dev/null || curl -s http://localhost:8080/services

clean: down
	docker compose rm -f
