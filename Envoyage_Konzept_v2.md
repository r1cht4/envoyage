# Envoyage — Konzept & Architektur (v2)

> Open-Source Reverse-Proxy- und Hosting-Plattform für Homelabber und Professionals.
> Inspiriert von Pangolin, gebaut auf Envoy, WireGuard und einem modularen Discovery-System.

---

## 1. Vision & Kernprinzipien

**Was ist Envoyage?**
Eine selbst gehostete Plattform, die es ermöglicht, Anwendungen sicher, schnell und komfortabel über verteilte Infrastruktur (Homeserver + VPS, nur VPS, nur Homeserver) erreichbar zu machen — mit Zero-Trust-Ansatz, automatischer Service-Discovery und feingranularer Zugriffskontrolle.

**Kernprinzipien:**

- **Secure by Default** — Verschlüsselung überall (WireGuard, mTLS), auch im Heimnetz
- **Echte Zero-Config** — Docker-Labels setzen → Service ist erreichbar. Kein Sidecar-Zwang
- **Modular & Erweiterbar** — Einzelmodus (nur VPS / nur Homeserver) bis Multi-Node-Cluster
- **Homelabber-First** — 5-Minuten-Setup, professionelle Features bei Bedarf zuschaltbar
- **Dual-Serving-Architektur** — Lokaler Traffic bleibt lokal, externer Traffic geht über VPS
- **Graceful Degradation** — Jede Komponente kann ausfallen, ohne das Gesamtsystem zu zerstören

---

## 2. Deployment-Topologien

### Topologie A: Homeserver + VPS (Primär-Szenario)

```
Internet
    │
    ▼
┌──────────────┐       WireGuard-Tunnel        ┌────────────────────┐
│   VPS Node   │◄──────────────────────────────►│   Home Node        │
│  (Edge)      │                                │  (Control Plane)   │
│              │   xDS-Sync (gRPC über WG)      │                    │
│  • Envoy     │◄───────────────────────────────│  • Envoy           │
│  • CertCache │                                │  • Control Plane   │
│  • WAF/ACL   │                                │  • DNS (Unbound)   │
│  • Auth Proxy│                                │  • Docker Discovery│
│              │                                │  • App 1..N        │
└──────────────┘                                └────────────────────┘
       ▲                                                ▲
       │ HTTPS                                          │ LAN (mTLS)
       │                                                │
   Externer User                              Lokaler User (direkt,
                                              ohne VPS-Hop)
```

**NAT-Traversal:**
Der Homeserver sitzt typischerweise hinter DS-Lite, Carrier-Grade NAT oder einem normalen Router ohne Portfreigaben. Deshalb gilt immer: **Homeserver = WireGuard-Client, VPS = WireGuard-Server.** Der Homeserver initiiert die Verbindung und hält sie mit `PersistentKeepalive=25` aufrecht. Das Control Plane berücksichtigt diese Asymmetrie automatisch beim Setup — der User muss sich darum nicht kümmern.

```
Homeserver (Client)                    VPS (Server)
┌────────────────┐                     ┌────────────────┐
│ wg0: 10.0.0.2  │────initiiert───────►│ wg0: 10.0.0.1  │
│                │  PersistentKeepalive │ Endpoint: :51820│
│ NAT/DS-Lite    │                     │ Öffentliche IP  │
│ (kein Problem) │◄────────────────────│                │
└────────────────┘   Bidirektional     └────────────────┘
                     (Tunnel steht)
```

### Topologie B: Nur VPS
- Alle Apps laufen auf dem VPS oder sind per WireGuard mit Remote-Nodes verbunden
- Control Plane sitzt auf dem VPS selbst

### Topologie C: Nur Homeserver
- Kein externer Zugang oder nur über DynDNS/Cloudflare-Tunnel
- Perfekt für rein lokale Setups mit LAN-Discovery

---

## 3. Komponentenarchitektur

### 3.1 Control Plane (Herzstück — läuft auf dem Homeserver)

Das **Control Plane** ist die zentrale Steuerungseinheit. Es verwaltet die gesamte Konfiguration und stellt sie dynamisch via Envoys xDS-API bereit.

**Technologie:** Go — starkes Ökosystem für Netzwerk-Tools, schnelle Entwicklung, gute WireGuard-/gRPC-Libraries. Zwingend basierend auf dem offiziellen **`go-control-plane`**-Repository (github.com/envoyproxy/go-control-plane).

**Warum `go-control-plane` als Basis?**
Envoy ist extrem strikt bei xDS. Fehlerhafte Konfigurationen führen zu NACK-Zyklen, im schlimmsten Fall droppt Envoy allen Traffic. Eine Eigenimplementierung der gRPC-xDS-Schnittstellen wäre fahrlässig — `go-control-plane` liefert die korrekte Snapshot-basierte State-Verwaltung, ACK/NACK-Handling und die richtigen Protobuf-Typen.

**Aufgaben:**

- **xDS-Server** (via `go-control-plane`) — Implementiert Envoys gRPC-basierte xDS-APIs (LDS, RDS, CDS, EDS, SDS). Jede Envoy-Instanz (Home + VPS) subscribt sich hier und erhält ihre Konfiguration dynamisch
- **Config Validator** — Jede Konfigurationsänderung durchläuft eine Validierungsschicht, bevor sie als neuer xDS-Snapshot gepusht wird. Prüft: Schema-Konformität, Referenz-Integrität (Cluster ↔ Endpoint ↔ Route), TLS-Konsistenz. Bei Fehler: Änderung wird abgelehnt, aktueller State bleibt bestehen
- **Service Registry** — Zentrale Datenbank aller registrierten Services (Name, Upstream-Adresse, Domain, TLS-Einstellungen, ACL-Regeln)
- **Docker Discovery** — Überwacht Docker Socket, erkennt Container mit Envoyage-Labels, registriert Services automatisch (Primärer Discovery-Modus, siehe 3.3)
- **Agent Coordinator** — Empfängt Registrierungen von optionalen Agents (für VMs, Remote-Hosts, erweiterte Kontrolle)
- **Zertifikatsverwaltung** — ACME-Client (Let's Encrypt) zur Zertifikatsanforderung, Speicherung und Verteilung über SDS (Secret Discovery Service)
- **WireGuard Manager** — Verwaltet WireGuard-Konfigurationen, Peer-Keys, IP-Zuweisung, erkennt Topologie-Asymmetrie (Homeserver = Client)
- **DNS Controller** — Steuert Unbound-Konfiguration für lokale DNS-Überschreibungen
- **Config Store** — SQLite (embedded) für persistente Konfiguration

```
┌──────────────────────────────────────────────────────────┐
│                     CONTROL PLANE                        │
│                                                          │
│  ┌──────────────────┐  ┌──────────────┐  ┌───────────┐  │
│  │  xDS Server      │  │  Config      │  │  ACME     │  │
│  │  (go-control-    │  │  Validator   │  │  Client   │  │
│  │   plane)         │  │  (pre-push)  │  │           │  │
│  └────────┬─────────┘  └──────┬───────┘  └─────┬─────┘  │
│           │                   │                 │        │
│  ┌────────▼───────────────────▼─────────────────▼─────┐  │
│  │              Config Store (SQLite)                  │  │
│  └────────────────────────────────────────────────────┘  │
│                                                          │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────────┐  │
│  │  Docker      │  │  WireGuard   │  │  DNS          │  │
│  │  Discovery   │  │  Manager     │  │  Controller   │  │
│  │  (primär)    │  │              │  │  (Unbound)    │  │
│  └──────────────┘  └──────────────┘  └───────────────┘  │
│                                                          │
│  ┌──────────────┐  ┌──────────────────────────────────┐  │
│  │  Agent       │  │  Web UI / API (Management)       │  │
│  │  Coordinator │  │  inkl. Auth-Provider-Integration  │  │
│  │  (opt.)      │  │                                  │  │
│  └──────────────┘  └──────────────────────────────────┘  │
└──────────────────────────────────────────────────────────┘
```

### 3.2 Envoy — Data Plane

Zwei Envoy-Instanzen: eine auf dem Homeserver, eine auf dem VPS.

**Home-Envoy:**
- Empfängt lokalen Traffic (über DNS-Überschreibung)
- Terminiert TLS lokal (mit denselben Zertifikaten wie der VPS)
- Routet direkt zu den lokalen Services (via mTLS oder plain, je nach Service-Config)
- Konfiguration kommt dynamisch vom lokalen Control Plane (localhost-xDS)

**VPS-Envoy:**
- Empfängt externen Traffic aus dem Internet
- Terminiert TLS (Zertifikate via SDS + lokaler Cache, siehe 3.6)
- Routet über den WireGuard-Tunnel zum Homeserver
- Bezieht Konfiguration vom Control Plane über den WireGuard-Tunnel (gRPC-xDS)
- Führt zusätzliche Sicherheitsfilter aus (Rate Limiting, WAF, Geo-Blocking)
- **Bei Tunnel-Ausfall:** Automatische Umschaltung auf statische Offline-Seite (siehe 3.6)

**Warum Envoy?**
- Dynamische Konfiguration über xDS — kein Reload/Restart nötig
- Eingebautes mTLS, Circuit Breaking, Retries, Rate Limiting
- Lua-Filter für benutzerdefinierte Logik (performant, einfach zu debuggen)
- Hervorragendes Observability (Prometheus-Metriken, Access Logs, Tracing)
- `ext_authz`-Filter für nahtlose Auth-Provider-Integration

### 3.3 Service Discovery — Zweistufiges Modell

**Das zentrale Design-Prinzip: Echte Zero-Config für den Normalfall, Agent als Opt-in für Sonderfälle.**

Ein Homelabber mit 40 Services will nicht 40 Compose-Files umschreiben. Deshalb ist Docker-Label-Discovery der **primäre** und **einzige** Modus für Phase 1. Der Agent kommt als Ergänzung in Phase 2.

#### Stufe 1: Docker-Label-Discovery (Standard, Phase 1)

Das Control Plane überwacht den Docker Socket und erkennt Container automatisch anhand von Labels.

```yaml
# Bestehende docker-compose.yml — nur Labels hinzufügen:
services:
  nextcloud:
    image: nextcloud:latest
    labels:
      envoyage.enable: "true"
      envoyage.domain: "cloud.example.com"
      envoyage.port: "80"
      # Optional:
      envoyage.auth: "required"           # Auth-Provider vorschalten
      envoyage.access: "public"           # public | vpn-only | link-only
      envoyage.healthcheck: "/status"     # Custom Health-Endpoint
      envoyage.tls.upstream: "true"       # Wenn App selbst TLS spricht
```

**Wie es funktioniert:**

```
Docker Daemon
    │
    │ Event-Stream (Container Start/Stop/Labels)
    ▼
┌────────────────────┐
│  Docker Discovery  │
│  (Control Plane)   │
│                    │
│  1. Container-Event erkannt
│  2. Labels parsen
│  3. Container-IP + Netzwerk ermitteln
│  4. Service in Registry eintragen
│  5. Config Validator prüft
│  6. xDS-Snapshot aktualisieren
│  7. DNS-Zone aktualisieren
└────────────────────┘
    │
    ├──► Home-Envoy (neuer Route/Cluster)
    ├──► VPS-Envoy  (neuer Route/Cluster via WG)
    └──► Unbound    (neue Local Zone)
```

**Verschlüsselung bei Label-Discovery:**
Ohne Agent gibt es kein mTLS bis zum Container. Dafür gibt es Envoy-seitig zwei Optionen:

- **Default:** Envoy routet über das Docker-Netzwerk direkt zum Container (plain HTTP). Für die meisten Homelabber ausreichend — der Traffic bleibt auf dem selben Host
- **Erhöht:** Envoy und die Ziel-Container teilen ein dediziertes Docker-Netzwerk. envoyage kann ein eigenes Netzwerk (`envoyage-mesh`) erstellen und Container automatisch daran anhängen. In Kombination mit Docker-Netzwerk-Isolation ist das ein guter Kompromiss
- **Maximum (Agent):** Für paranoidere Setups → Stufe 2

#### Stufe 2: Agent (Opt-in, ab Phase 2)

Für Szenarien, die Docker-Labels nicht abdecken:

| Szenario | Warum Agent nötig |
|---|---|
| VMs / Bare-Metal-Services | Kein Docker, keine Labels |
| Remote-Hosts (anderer Server) | Nicht am selben Docker-Daemon |
| mTLS bis zur App | Zero-Trust im LAN |
| Erweiterte Health-Checks | Custom Logik, App-spezifisch |
| Sidecar-Proxy-Muster | Service Mesh innerhalb des Hosts |

```yaml
# Nur wenn wirklich nötig:
services:
  envoyage-agent:
    image: envoyage/agent:latest
    environment:
      envoyage_TOKEN: "abc123..."
      envoyage_SERVICE: "my-vm-app"
      envoyage_UPSTREAM: "192.168.1.100:8080"
      envoyage_DOMAIN: "vm-app.example.com"
```

**Agent-Funktionen:**
- **Auto-Registration** — Meldet sich beim Control Plane an (gRPC + Token-Auth)
- **mTLS-Sidecar-Proxy** — Verschlüsselt Traffic zwischen Envoy und Ziel-App
- **Health Checks** — Meldet Gesundheitsstatus ans Control Plane
- **Metadata** — Labels, Routing-Regeln, Custom-Konfiguration

**mTLS-Architektur (mit Agent):**

```
Home-Envoy ──── mTLS ────► Agent (Sidecar) ──── localhost ────► App
     ▲                          │
     │                          ▼
  Interne CA                Health-Check
  (Control Plane)           an Control Plane
```

Das Control Plane betreibt eine interne CA (Smallstep/step-ca oder eigene Implementierung). Automatische Zertifikatsausstellung für Agents (SPIFFE-kompatible Identitäten empfohlen), kurze Laufzeiten (24h) mit automatischer Rotation.

### 3.4 DNS-Subsystem (Unbound + optional Pi-hole)

**Ziel:** Lokale Clients sollen `app.example.com` direkt auf die LAN-IP des Homeservers auflösen, nicht über den VPS (Split-Horizon DNS als First-Class Citizen).

```
┌─────────────────────────────────────────┐
│          DNS-Subsystem (Homeserver)      │
│                                         │
│  ┌──────────┐     ┌──────────────────┐  │
│  │ Pi-hole  │────►│    Unbound       │  │
│  │ (opt.)   │     │  (Resolver)      │  │
│  │ Ad-Block │     │                  │  │
│  └──────────┘     │  Local Zones:    │  │
│                   │  app.example.com │  │
│                   │   → 192.168.1.50 │  │
│                   └──────────────────┘  │
│                          ▲              │
│                          │              │
│  Control Plane ──────────┘              │
│  (aktualisiert Zonen automatisch        │
│   bei jedem Service-Event)              │
└─────────────────────────────────────────┘
```

**Automatische Konfiguration:**
1. Docker-Label-Discovery erkennt neuen Container mit Domain `app.example.com`
2. Control Plane aktualisiert Unbound Local-Zone: `app.example.com → 192.168.1.X`
3. Lokale Clients (Homeserver als DNS) lösen die Domain direkt lokal auf
4. Externe Clients erreichen denselben Service über den VPS (öffentlicher A-Record → VPS-IP)

**Discovery im Heimnetz:**
- **mDNS/Avahi** als optionale Ergänzung — Homeserver bewirbt sich als `envoyage.local`, hilfreich für initiales Setup
- **DHCP-Integration** (optional) — Homeserver kann sich als DNS-Server via DHCP-Option verteilen

### 3.5 Auth-Provider-Integration (Kern-Feature, nicht optional)

**Design-Entscheidung:** Statt "Authentik optional einbauen" wird envoyage ein **generisches Auth-Provider-Interface** im Kern haben. Der User bringt seinen eigenen IdP mit (Authentik, Authelia, Keycloak, etc.) — envoyage konfiguriert den `ext_authz`-Filter automatisch.

**UI-Integration:**

```
Service-Einstellungen: cloud.example.com
┌──────────────────────────────────────────────┐
│  Zugriffskontrolle                           │
│                                              │
│  Modus: [▼ Auth erforderlich]                │
│                                              │
│  Auth Provider: [▼ Mein Authentik]           │
│  ┌─ Provider konfigurieren ──────────────┐   │
│  │  Name: Mein Authentik                 │   │
│  │  Typ:  [▼ Forward Auth]               │   │
│  │  URL:  https://auth.example.com/...   │   │
│  │  [Erweitert: OIDC Client ID/Secret]   │   │
│  └───────────────────────────────────────┘   │
│                                              │
│  Zusätzlich:                                 │
│  ☑ Auch CrowdSec-Prüfung                    │
│  ☐ Geo-Blocking aktiv                        │
│  ☐ Rate Limiting (custom)                    │
└──────────────────────────────────────────────┘
```

**Technisch:**

```
Externer Request
      │
      ▼
  VPS-Envoy
      │
      ├──► ext_authz (gRPC/HTTP) ──► Auth Provider (Authentik/Authelia/...)
      │                                    │
      │ ◄──────── Allow/Deny ──────────────┘
      │
      ▼ (bei Allow)
  WireGuard → Home-Envoy → App
```

- Envoy nutzt den `ext_authz`-Filter, der pro Route individuell konfigurierbar ist
- Auth Provider werden einmal global registriert, dann pro Service zugewiesen
- Zugriffsmodi pro Service: **Öffentlich** / **Auth erforderlich** / **VPN-Only** / **Link-Only** / **Custom ACL**
- Für Homelabber ohne eigenen IdP: eingebauter Basic-Auth-Modus als Minimum

### 3.6 VPS-Node: Resilience & Graceful Degradation

Der VPS ist ein "Edge Node" — zustandslos im Normalbetrieb, aber mit intelligentem Caching für Ausfallszenarien.

**Kernproblem: Was passiert, wenn der WireGuard-Tunnel ausfällt?**

Ohne Gegenmaßnahmen:
- Envoy hat keine Upstream-Verbindung → 503 für alle Requests
- Bei VPS-Restart: Keine Zertifikate (SDS unerreichbar) → TLS-Handshake schlägt fehl → nicht mal eine Fehlerseite
- User sieht: Connection Timeout oder Browser-Warnung

**Lösung: Edge Bootstrapper + Fallback-System**

Envoy kann keine verschlüsselten Blobs vom Dateisystem lesen. Es erwartet entweder Klartext-PEM auf Disk oder Secrets via SDS (gRPC). Deshalb benötigt der VPS einen eigenen Hilfsprozess: den **Edge Bootstrapper**.

```
┌──────────────────────────────────────────────────────────────┐
│                        VPS NODE                              │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  Edge Bootstrapper (Go Binary, startet vor Envoy)      │  │
│  │                                                        │  │
│  │  1. Encrypted State Cache lesen (AES-256-GCM)          │  │
│  │  2. Entschlüsseln (Bootstrap-Secret aus Env/File)      │  │
│  │  3. Zertifikate → tmpfs (/run/envoyage/certs/)        │  │
│  │  4. Lokalen SDS-Server starten (Unix Socket)           │  │
│  │  5. Letzten xDS-Snapshot als Envoy-Bootstrap bereit-   │  │
│  │     stellen (statische Fallback-Config)                │  │
│  │  6. Envoy starten                                      │  │
│  │  7. Bei Tunnel-Recovery: auf Remote-SDS umschalten     │  │
│  └──────────┬─────────────────────────────────────────────┘  │
│             │ Unix Socket (SDS)                              │
│             ▼                                                │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  Envoy (Data Plane)                                    │  │
│  │                                                        │  │
│  │  SDS-Quelle (Priorität):                               │  │
│  │  1. Remote SDS via WG-Tunnel (Control Plane)           │  │
│  │  2. Lokaler SDS via Unix Socket (Edge Bootstrapper)    │  │
│  │                                                        │  │
│  │  Route-Hierarchie pro Service:                         │  │
│  │  1. Upstream via WG-Tunnel (normal)                    │  │
│  │  2. Bei Timeout → Lua-Filter → Offline-Seite           │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  Encrypted State Cache (Disk)                          │  │
│  │                                                        │  │
│  │  • TLS-Zertifikate (AES-256-GCM)                      │  │
│  │  • Letzter xDS-Snapshot (Listener, Routes, Clusters)   │  │
│  │  • Auth-Provider-Config                                │  │
│  │  • Statische Offline-Seiten pro Service                │  │
│  │                                                        │  │
│  │  Key: Abgeleitet vom Bootstrap-Secret                  │  │
│  │  (gesetzt bei initialem VPS-Setup, in Env oder File)   │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                              │
│  ┌────────────┐ ┌────────────┐ ┌───────────────────┐        │
│  │  ACME      │ │  CrowdSec  │ │  Link Service     │        │
│  │  Agent     │ │  Bouncer   │ │  (Temp. Zugang)   │        │
│  └────────────┘ └────────────┘ └───────────────────┘        │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  WireGuard (Client-Endpoint für User-Devices)          │  │
│  │  + Tunnel zum Homeserver                               │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  Health Monitor                                        │  │
│  │  • Prüft Tunnel-Status alle 5s                         │  │
│  │  • Triggert Fallback/Recovery-Logik                    │  │
│  │  • Sendet Alerts (Webhook/Push)                        │  │
│  └────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────┘
```

**Startsequenz bei VPS-Boot (Tunnel offline):**

```
Systemd startet envoyage-edge.service
      │
      ▼
Edge Bootstrapper startet
      │
      ├──► Encrypted Cache vorhanden?
      │    ├─ Ja → Entschlüsseln → tmpfs + lokaler SDS
      │    └─ Nein → Envoy startet ohne Certs (nur HTTP :80)
      │
      ▼
Envoy startet
      │
      ├──► Verbindet sich zum lokalen SDS (Unix Socket)
      │    → Lädt gecachte Zertifikate → HTTPS funktioniert
      │
      ├──► Versucht Remote-xDS über WG-Tunnel
      │    → Fehlschlag → nutzt gecachten Snapshot (Offline-Seiten)
      │
      ▼
User sieht: Saubere "Service Offline"-Seite über HTTPS
(statt Connection Timeout oder Browser-Warnung)
```

**Ablauf bei Tunnel-Ausfall:**

```
Tunnel bricht ab
      │
      ▼
Health Monitor erkennt (5s)
      │
      ├──► Alert an User (Webhook/ntfy/Push)
      │
      ▼
Envoy-Upstreams werden als unhealthy markiert
      │
      ▼
Lua-Filter greift bei jedem Request:
  → Liefert per-Service Offline-Seite aus
  → HTTP 503 mit Retry-After Header
  → Custom Branding (envoyage-Logo + Servicename)
      │
      ▼
Tunnel reconnected (WG PersistentKeepalive)
      │
      ▼
Health Monitor erkennt Recovery
  → Envoy-Upstreams wieder healthy
  → Normaler Betrieb
  → Recovery-Alert
```

**Zertifikats-Caching (via Edge Bootstrapper):**
- Edge Bootstrapper cached Zertifikate lokal verschlüsselt (AES-256-GCM)
- Entschlüsselungskey wird beim initialen VPS-Setup gesetzt (Bootstrap-Secret)
- Bei VPS-Restart ohne Tunnel: Edge Bootstrapper entschlüsselt Cache → schreibt Certs auf tmpfs → startet lokalen SDS (Unix Socket) → Envoy verbindet sich und lädt Certs → TLS funktioniert → Offline-Seite wird angezeigt
- Envoy sieht niemals verschlüsselte Daten — nur Klartext via SDS oder tmpfs
- Cache wird bei jedem SDS-Update vom Control Plane aktualisiert

### 3.7 Zertifikats-Management: Wo werden Certs angefordert?

**Wichtige Designentscheidung:** Zertifikate werden am **VPS** angefordert (ACME HTTP-01 Challenge), aber vom **Control Plane** verwaltet.

```
1. User legt neuen Service an (cloud.example.com)
2. Control Plane signalisiert VPS-ACME-Agent: "Zertifikat anfordern"
3. VPS löst HTTP-01 Challenge (Port 80 ist dort offen)
4. VPS sendet Zertifikat + Key über WG-Tunnel an Control Plane
5. Control Plane speichert in Config Store
6. Control Plane verteilt via SDS an beide Envoys (Home + VPS)
7. Edge Bootstrapper auf VPS cached Zertifikat verschlüsselt lokal
```

Für DNS-01-Challenge (Wildcard-Certs): Control Plane kann direkt über DNS-Provider-API validieren — unabhängig vom VPS.

---

## 4. Sicherheit & Zugriffskontrolle

### 4.1 Zero-Trust-Netzwerkmodell

```
Schicht 1: WireGuard              — Verschlüsselter Tunnel VPS ↔ Homeserver
Schicht 2: mTLS (Envoy ↔ Agent)   — Verschlüsselung im LAN (opt-in pro Service)
Schicht 3: ext_authz               — Identitätsprüfung am Edge (VPS)
Schicht 4: App-Level Auth          — Eigene Authentifizierung der App
```

**Verschlüsselungs-Stufen (User wählt pro Service):**

| Stufe | Was ist verschlüsselt | Aufwand | Für wen |
|---|---|---|---|
| **Basis** | Internet → VPS (TLS) + VPS → Home (WG) | Null (Docker-Labels) | Die meisten Homelabber |
| **Erhöht** | + Isoliertes Docker-Netzwerk | Minimal (automatisch) | Security-bewusste User |
| **Maximum** | + mTLS Envoy → Agent → App | Agent deployen | Professionelle Setups, VMs |

### 4.2 CrowdSec + Envoy-native Security

Statt fail2ban (log-basiert, langsam) → **CrowdSec**:

- **Community-Blocklisten** — Geteilte Threat Intelligence
- **Envoy-Integration** — CrowdSec Bouncer als `ext_authz`-Backend (gleicher Mechanismus wie Auth Provider, gestackt)
- **Szenarien** — Erkennung von Brute-Force, Scanning, L7-DDoS
- **Leichtgewichtig** — Go-basiert, niedriger Ressourcenverbrauch

**Envoy-native Features (immer aktiv):**
- Rate Limiting (Global + Local)
- Connection Limits
- Geo-IP-basiertes Routing/Blocking (via MaxMind GeoIP-Filter)
- Request-Header-basierte Regeln

### 4.3 Temporäre Zugangslinks

Sichere Freigabe einzelner Services ohne VPN oder Passwort:

```
User erstellt Link im UI:
  → Service: nextcloud.example.com
  → Gültig: 2 Stunden
  → Max. IPs: 3
  → Optionale Passwort-Absicherung

System generiert:
  → https://nextcloud.example.com?_nxg_token=abc123...

Ablauf:
  1. Besucher öffnet Link
  2. VPS-Envoy prüft Token (Lua-Filter, effizient)
  3. Bei gültigem Token: IP wird in Allowlist aufgenommen
  4. Weitere Requests von dieser IP passieren ohne Token
  5. Nach Ablauf: IP wird entfernt, Token invalidiert
```

**Erweiterungen:**
- Bandbreitenlimit pro Link
- Revoke-Button im UI
- Zugriffs-Log pro Link
- Notifications bei Nutzung (Webhook/Push)

### 4.4 WireGuard Client-Management

Für vertraute Geräte von unterwegs:

- **Profil-Generator** — UI zum Erstellen von WireGuard-Configs
- **Granulare Policies** — Pro Client: "Voller Zugriff", "Nur Service X und Y", "Nur bestimmte Ports"
- **Device Groups** — "Mein Handy", "Arbeitslaptop", "Familie"
- **Auto-Expiry** — Optionale Zeitbeschränkung
- **QR-Code-Export** — Für mobile WireGuard-Clients
- **Kill Switch** — Einzelne Clients sofort sperren

---

## 5. Management UI

### 5.1 Dashboard

```
┌─────────────────────────────────────────────────────────┐
│  envoyage Dashboard                              ⚙  👤 │
├─────────────────────────────────────────────────────────┤
│                                                         │
│  ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌──────────────┐  │
│  │ Services│ │  Nodes  │ │ Clients │ │  Tunnel      │  │
│  │   12 ✅  │ │  2/2 🟢 │ │  5 VPN  │ │  ✅ 12ms     │  │
│  └─────────┘ └─────────┘ └─────────┘ └──────────────┘  │
│                                                         │
│  Services                                   [+ Add]    │
│  ┌─────────────────────────────────────────────────┐    │
│  │ 🟢 cloud.example.com   │ Docker │ Auth    │ ⚙  │    │
│  │ 🟢 git.example.com     │ Docker │ VPN     │ ⚙  │    │
│  │ 🟡 plex.example.com    │ Docker │ Public  │ ⚙  │    │
│  │ 🔵 vm-app.example.com  │ Agent  │ Link    │ ⚙  │    │
│  └─────────────────────────────────────────────────┘    │
│                                                         │
│  Security (24h)                    Quick Actions        │
│  ┌─────────────────────────┐  ┌──────────────────────┐  │
│  │ 🛡 847 blocked           │  │ 🔗 Temp Link erstellen│  │
│  │ 🔑 12 failed auth        │  │ 📱 VPN-Profil anlegen │  │
│  │ 🔗 3 temp links active   │  │ ➕ Service hinzufügen │  │
│  └─────────────────────────┘  └──────────────────────┘  │
└─────────────────────────────────────────────────────────┘
```

### 5.2 Tech Stack UI

- **Frontend:** React + TailwindCSS (SPA), ausgeliefert vom Control Plane
- **Backend-API:** Go (integriert ins Control Plane)
- **Echtzeit-Updates:** WebSocket/SSE für Live-Status (Tunnel-Health, Service-Status, Security Events)

---

## 6. Erweiterte Features

### 6.1 Automatisches DNS-Management (extern)

Neben lokaler DNS-Überschreibung auch **externe DNS-Records** automatisch verwalten:

- Integration mit Cloudflare, Hetzner DNS, Route53 etc. via API
- Bei neuem Service: A-Record → VPS-IP wird automatisch erstellt
- Bei Entfernung: Record wird gelöscht
- Wildcard-Support (*.apps.example.com)

### 6.2 Backup & Disaster Recovery

- **Config-Export/Import** — Gesamte Konfiguration als verschlüsseltes Backup
- **Automatic Snapshots** — Regelmäßige Sicherung der SQLite-DB + WireGuard-Keys
- **Recovery-Modus** — VPS zeigt bei Homeserver-Ausfall Offline-Seiten (via State Cache)

### 6.3 Observability Stack

- **Metriken:** Envoy → Prometheus → Grafana (vorkonfigurierte Dashboards)
- **Logs:** Envoy Access Logs → Loki oder integriertes Log-Viewing im UI
- **Alerting:** Webhook-basiert (Discord, Slack, Gotify, ntfy)
- **Integrierter Diagnostics-Check:** LAN vs. VPS Latenz-Vergleich, Tunnel-Gesundheit, DNS-Auflösung-Test

### 6.4 Lua-Filter-Bibliothek (statt WASM-Plugin-Store)

Envoy unterstützt Lua-Filter nativ — das ist für Custom-Routing performant genug und dramatisch einfacher als WASM:

- Vorgefertigte Lua-Snippets für gängige Aufgaben (Header-Manipulation, Redirects, Maintenance-Mode)
- UI-Editor zum Aktivieren/Bearbeiten pro Route
- Community kann Snippets beitragen
- **WASM bleibt als Zukunftsoption** für Professional-Tier, aber nicht im initialen Scope

### 6.5 Multi-Node / Clustering (Zukunft)

- Mehrere Homeserver an einem Control Plane
- Service-Placement und Failover
- Geographic Routing über mehrere VPS

---

## 7. Tech Stack Zusammenfassung

| Komponente | Technologie | Begründung |
|---|---|---|
| **Data Plane** | Envoy Proxy | Dynamische xDS-Config, mTLS, Lua-Filter, Observability |
| **Control Plane** | Go + go-control-plane | xDS-korrekt, performant, gRPC-nativ |
| **Edge Bootstrapper** | Go (VPS-Binary) | Lokaler SDS, Cache-Entschlüsselung, Envoy-Lifecycle |
| **Discovery (primär)** | Docker Socket Listener | Zero-Config: Labels setzen → fertig |
| **Discovery (erweitert)** | Go Agent (Static Binary) | Für VMs, Remote-Hosts, mTLS |
| **Tunnel** | WireGuard | Schnell, sicher, minimaler Overhead |
| **DNS** | Unbound (+ Pi-hole optional) | Split-Horizon DNS als First-Class Citizen |
| **Interne CA** | step-ca (Smallstep) | Automatische mTLS-Zertifikate (für Agent-Modus) |
| **Auth** | Generisches ext_authz-Interface | Authentik, Authelia, Keycloak — User bringt seinen IdP mit |
| **Security** | CrowdSec + Envoy-native Filter | Community Threat Intel + L7-Schutz |
| **Config Store** | SQLite (embedded) | Einfach, keine externe DB nötig |
| **UI** | React + TailwindCSS | Modern, schnell, große Community |
| **Observability** | Prometheus + Grafana (optional) | Standard-Stack, Envoy liefert Metriken nativ |

---

## 8. Installationsablauf (Ziel-UX)

### Homeserver-Setup (< 5 Minuten)

```bash
# 1. envoyage installieren
curl -fsSL https://get.envoyage.dev | bash

# 2. Interaktiver Setup-Wizard
#    → Domain eingeben (example.com)
#    → VPS-IP eingeben (oder "kein VPS" / "nur VPS")
#    → Admin-Passwort setzen
#    → DNS-Provider für automatische Records (optional)
#    → Auth Provider URL (optional, später nachrüstbar)

# 3. Output:
#    → WireGuard-Config für VPS (Copy-Paste oder One-Liner)
#    → Dashboard: https://envoyage.local:8443 (initial)
```

### VPS-Setup (< 2 Minuten)

```bash
# Auf dem VPS:
curl -fsSL https://get.envoyage.dev/edge | bash

# WireGuard-Config einfügen (oder One-Liner aus Homeserver-UI)
# → VPS verbindet sich automatisch
# → Envoy startet, bezieht Config via xDS
# → Zertifikate werden angefordert und gecacht
# → Dashboard jetzt unter https://envoyage.example.com
```

### Service hinzufügen (10 Sekunden)

```yaml
# Bestehende docker-compose.yml:
services:
  nextcloud:
    image: nextcloud:latest
    labels:                              # <- nur das hinzufügen
      envoyage.enable: "true"
      envoyage.domain: "cloud.example.com"
```

```bash
docker compose up -d
# → Automatisch erkannt
# → Domain konfiguriert (lokal + extern)
# → TLS-Zertifikat angefordert
# → Im Dashboard sichtbar
```

---

## 9. Roadmap

### Phase 1 — Foundation (MVP)
- [ ] Control Plane mit xDS-Server (Go + go-control-plane)
- [ ] Config Validator (Pre-Push-Validation)
- [ ] Docker-Label-Discovery (primärer Discovery-Modus)
- [ ] Envoy-Konfiguration für Home + VPS
- [ ] WireGuard-Tunnel-Setup (Homeserver = Client, VPS = Server)
- [ ] Unbound DNS mit automatischen Local Zones (Split-Horizon)
- [ ] ACME-Zertifikatsverwaltung (VPS-seitig, SDS-Verteilung)
- [ ] Edge Bootstrapper (VPS): Lokaler SDS, Encrypted State Cache, Envoy-Lifecycle
- [ ] Graceful Degradation (Lua-Filter Offline-Seite bei Tunnel-Ausfall)
- [ ] Minimales Web-UI (Service-Übersicht, Basis-Config)
- [ ] CLI-basiertes Setup mit interaktivem Wizard

### Phase 2 — Security & Access Control
- [ ] Auth-Provider-Integration (generisches ext_authz-Interface im UI)
- [ ] CrowdSec-Integration
- [ ] Temporäre Zugangslinks
- [ ] WireGuard Client-Management + QR-Codes
- [ ] envoyage Agent (Go Binary) für VMs und mTLS
- [ ] Interne CA (Smallstep) für Agent-mTLS
- [ ] Geo-Blocking, Rate Limiting UI
- [ ] Feingranulare ACLs pro Service

### Phase 3 — Polish & Observability
- [ ] Vollständiges Dashboard mit Echtzeit-Status
- [ ] Prometheus/Grafana-Integration
- [ ] Alerting (Webhooks: Discord, Gotify, ntfy)
- [ ] Backup/Restore
- [ ] Automatisches externes DNS-Management
- [ ] Lua-Filter-Bibliothek mit UI-Editor
- [ ] Integrierter Diagnostics-Check (LAN vs. VPS, DNS, Tunnel)

### Phase 4 — Advanced
- [ ] Multi-Node-Clustering
- [ ] Einzelmodus (nur VPS / nur Homeserver)
- [ ] A/B-Testing, Canary Deployments
- [ ] Mobile App / PWA
- [ ] WASM-Filter (Professional-Tier)

---

## 10. Abgrenzung zu Pangolin & Alternativen

| Feature | Pangolin | Cloudflare Tunnels | **envoyage** |
|---|---|---|---|
| Proxy | Traefik/Caddy | Cloudflare Edge | **Envoy** (xDS, dynamisch) |
| TLS Termination | Eigener Server | **Cloudflare** (Drittpartei!) | **Eigener VPS** (volle Kontrolle) |
| Konfiguration | File-basiert | Dashboard (proprietär) | **Dynamisch** (xDS, kein Restart) |
| LAN-Optimierung | Nein | Nein | **Split-Horizon DNS** |
| Discovery | Manuell | Connector-basiert | **Docker-Labels** (Zero-Config) |
| mTLS im LAN | Nein | Nein | **Opt-in** (Agent-Modus) |
| Auth-Integration | Begrenzt | Access (proprietär) | **Generisches ext_authz** |
| Temp. Links | Nein | Nein | **Ja** (zeit-/IP-basiert) |
| Graceful Degradation | Nein | Cloudflare-abhängig | **Encrypted State Cache** |
| Datenhoheit | Ja | **Nein** | **Ja** |
| Multi-Node | Nein | Ja (Cloudflare-Infra) | **Geplant** |
| Security | Basis | Cloudflare WAF | **CrowdSec** + Envoy-nativ |
