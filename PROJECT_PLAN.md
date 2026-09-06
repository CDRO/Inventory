Eine schlüsselfertige Software, die **alle** diese Anforderungen in einem einzigen System out-of-the-box vereint, existiert derzeit nicht auf dem Markt.

Bestehende Open-Source- und Enterprise-Lösungen decken jeweils nur Teilbereiche ab:

* **Grocy** (Self-Hosted): Bietet Vorratsverwaltung, Verfallsdaten, Einkaufslisten und Mindestbestände, besitzt aber keine native Vision-KI zur Erkennung mehrschichtiger 50MP-Regalfotos, kein 3D-Raumverständnis und keinen interaktiven Bild-Vorschlags-Workflow.
* **SnapFind / Scanlily / ShelfLily**: Bieten KI-Bilderkennung für Kisten und Regale via Smartphone, unterstützen jedoch weder den Einkaufslisten-Import-Workflow mit Bildsuch-Vorschlägen noch Konsum-Fotos mit manueller Mengen-Übersteuerung.

Die Umsetzung erfordert eine maßgeschneiderte Anwendung.

---

## Architektur & Hosting auf Synology DSM

### Remote-Zugriff & Deployment

* **Staging-Phase:** Lokale Entwicklung und Container-Tests mittels Docker Desktop auf dem PC.
* **Produktiv-System:** Hosting im Docker (Container Manager) auf der Synology NAS.
* **Weltweiter Zugriff:**
1. **Tailscale (Empfohlen):** Installierbar als Synology-Paket. Baut ein privates WireGuard-Netzwerk auf, ohne Ports am Router zu öffnen.
2. **Cloudflare Tunnel (Alternative):** Stellt die Web-App sicher über eine HTTPS-Domain bereit, inklusive Authentifizierung (z. B. Cloudflare Access).



### Recommended Tech-Stack

* **Frontend:** Next.js / React als Progressive Web App (PWA) für nahtlosen Smartphone-Kamera-Upload.
* **Backend:** Python FastAPI (Verarbeitung von Vision-Prompts, Bounding-Boxes, JSON-Parsing).
* **Datenbank:** PostgreSQL (Speicherung von 3D-Standort-Bäumen, Inventarhistorie und Mindestbeständen).
* **KI-Vision & Match-Engine:** **Google Gemini 1.5 Flash** (sehr günstig, extrem großes Kontextfenster für 50MP-Bilder) oder **OpenAI GPT-4o**.
* **Bildsuche-API:** DuckDuckGo Image Search / SerpAPI für das Abrufen der 3 Bildvorschläge.

---

## Spezifikation für den KI-Agenten (PRD)

Kopiere den folgenden Block direkt in den KI-Code-Generator (z. B. Cursor, Claude 3.5 Sonnet oder AutoGPT), um die Anwendung bauen zu lassen:

```markdown
# Product Requirement Document (PRD): AI-Powered Spatial Home Inventory System

## 1. System Overview
Build a self-hosted, full-stack home inventory application. The app uses Multimodal Vision AI to index physical items from photos, manage inventory levels with 3D spatial awareness, reconcile shopping lists, and track expiration dates/reorder points.

---

## 2. Technical Stack
- **Frontend**: Next.js (React), TailwindCSS, PWA support (camera capture integration)
- **Backend**: Python FastAPI
- **Database**: PostgreSQL
- **External APIs**: 
  - Vision LLM: Google Gemini 1.5 Flash or OpenAI GPT-4o API
  - Image Search: DuckDuckGo / SerpAPI / Unsplash API
- **Deployment**: Docker Compose (Target platform: Synology DSM NAS via Container Manager)

---

## 3. Core Features & Functional Requirements

### Feature 1: Spatial 3D Location Hierarchy
- **Data Structure**: Hierarchical tree model for locations (e.g., `Location -> Room -> Container/Shelf -> Layer/Zone`).
- **Example**: `Basement -> Right Shelf -> Shelf Layer 2 -> Front-Right`.
- Items can exist in multiple physical locations simultaneously (e.g., Cucumber Jars in `Fridge` vs. `Cellar`).

### Feature 2: High-Resolution Shelf Photo Ingestion
- Allow uploading/capturing high-resolution photos (up to 50MP).
- Send image to Vision LLM to segment items, infer quantities, and propose locations within the 3D shelf structure.
- **Review UI**: Display detected items with an interactive layer/quantity editor allowing users to manually override item counts.

### Feature 3: Smart Shopping List Reconciliation
- Upload text or image of a shopping list.
- Match items against existing database using semantic search.
- Categorize list items into three states:
  1. **Exact Match**: Generate an auto-update inventory proposal for review.
  2. **New Item**: Trigger external image search. Provide **3 visual suggestions** (1 Icon/Vector, 2 Internet Product Images). User can select one, upload a custom picture, or re-trigger search.
  3. **Ambiguous Item**: Present manual selection UI to map to existing inventory, create a new item with image search, or define manually.

### Feature 4: Expiration & Item Type Classification
- **Classification**: Distinguish between `Perishable` (e.g., fruits, dairy), `Long-shelf-life` (canned goods), and `Non-perishable` (plush toys, trading cards, collectibles).
- **Auto-Expiry Assignment**: Every inventory addition defaults to an estimated expiration date based on category rules, fully editable or removable by the user.
- Sort and filter views by expiration urgency.

### Feature 5: Quick Consumption Logging
- User uploads photo of consumed item(s) (e.g., 1 toilet paper roll photo).
- AI identifies product; UI displays a delta proposal.
- **Override Control**: User can adjust count manually (e.g., photo shows 1 roll, but user changes decrement count to 3).

### Feature 6: Reorder & Minimum Stock Management
- Support tracking products with current quantity = 0.
- Define `minimum_threshold` per item.
- Dashboard highlights expiring products and low-stock items.
- Generate and export a shopping list (CSV/PDF) containing items below threshold.

### Feature 7: Reporting & Analytics
- Visual dashboard showing:
  - Total items stored.
  - Heatmap/Distribution of items per location.
  - Inventory turnover frequency and change log over time.

---

## 4. Database Schema (PostgreSQL Draft)

```sql
CREATE TABLE locations (
    id SERIAL PRIMARY KEY,
    parent_id INT REFERENCES locations(id),
    name VARCHAR(255) NOT NULL,
    description TEXT
);

CREATE TABLE products (
    id SERIAL PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    category VARCHAR(100),
    is_perishable BOOLEAN DEFAULT TRUE,
    min_stock INT DEFAULT 0,
    image_url TEXT,
    icon_name VARCHAR(100)
);

CREATE TABLE inventory_batches (
    id SERIAL PRIMARY KEY,
    product_id INT REFERENCES products(id),
    location_id INT REFERENCES locations(id),
    quantity INT NOT NULL DEFAULT 1,
    expiration_date DATE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE inventory_logs (
    id SERIAL PRIMARY KEY,
    product_id INT REFERENCES products(id),
    change_qty INT NOT NULL,
    reason VARCHAR(50), -- 'purchase', 'consumption', 'audit'
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

```

---

## 5. Docker Deployment Spec (`docker-compose.yml`)

```yaml
version: '3.8'

services:
  app-backend:
    build: ./backend
    ports:
      - "8000:8000"
    environment:
      - DATABASE_URL=postgresql://user:password@db:5432/inventory
      - GEMINI_API_KEY=${GEMINI_API_KEY}
    restart: always

  app-frontend:
    build: ./frontend
    ports:
      - "3000:3000"
    environment:
      - NEXT_PUBLIC_API_URL=http://localhost:8000
    restart: always

  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: user
      POSTGRES_PASSWORD: password
      POSTGRES_DB: inventory
    volumes:
      - pgdata:/var/lib/postgresql/data
    restart: always

volumes:
  pgdata:

```

```

``` ö