// Package httpapi owns the HTTP surface: the chi router, the JSON handlers,
// and the middleware that decides authorization
// (docs/specs/04-backend-api-conventions.md).
//
// Three rules of the package are structural rather than conventional, because
// each of them fails silently when it is left to a handler to remember:
//
//   - **One error serializer.** errors.go is the only place an error envelope
//     is produced and the only place debug_reason can be attached. A handler
//     describes a Failure and hands it over; it has no way to write an
//     envelope itself, so it cannot leak an internal reason in production.
//     chi's own 404 and 405 are routed through it too, and so is every miss
//     the static catch-all sees — see staticHandler for why that one matters
//     to the admin area as much as to the error format.
//   - **Three authorization gates, and nowhere else.** RequireSession,
//     RequireAdmin and RequireStorageMember in middleware.go decide access. No
//     handler performs its own check. RequireAdmin re-reads is_admin from the
//     database on every request, and every refusal — unknown storage,
//     inaccessible storage, malformed id, the whole admin area — is the same
//     404, byte for byte. The session gate has two refusal renderers over one
//     lookup — the error envelope for anything a script calls, a redirect to
//     the login page for the routes a browser navigates to
//     (docs/specs/29-first-run-admin-guidance.md) — which is a difference in
//     how a refusal is rendered, not a fourth gate deciding anything.
//   - **One upload path.** ReadImageUpload in upload.go strips metadata from
//     every image entering the system and generates the filename itself.
//
// The storage-scoped routes — the location tree and the batch operations of
// docs/specs/06-vision-shelf-ingestion.md — are registered on one sub-router
// carrying the gate chain, so a new route under /api/storages/{storage_id} is
// protected by the act of being added.
//
// The admin area — the /admin page and /api/admin/* — is likewise one group
// behind RequireSession → RequireAdmin, so the HTML and JSON halves cannot be
// gated differently.
//
// Every session-gated group also carries the Idempotency middleware, after its
// gates, so any write carrying an Idempotency-Key is safe for an offline client
// to retry (idempotency.go).
package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/admin"
	"github.com/CDRO/Inventory/internal/config"
)

// DBPinger reports whether the database is reachable. It is the narrow slice
// of *store.Store that the readiness endpoint needs, declared here so the
// handler can be tested without a database.
type DBPinger interface {
	Ping(ctx context.Context) error
}

// AdminVisionChecker is the fuller slice of *vision.Checker the admin
// settings routes and the /admin AI-model banner need — beyond
// VisionReporter's bare Status, they also resolve which model is effective,
// list what the provider currently offers, and drop the cached list after an
// admin corrects the model so the very next check reflects it
// (docs/specs/01-architecture-and-deployment.md's AI model resilience).
type AdminVisionChecker interface {
	EffectiveModel(ctx context.Context) (string, error)
	Models(ctx context.Context) ([]string, error)
	Status(ctx context.Context) string
	Invalidate()
}

// VisionReporter reports the vision subsystem's status, either
// vision.StatusOK or vision.StatusModelUnavailable.
type VisionReporter interface {
	Status(ctx context.Context) string
}

// APIStore is everything the storage-scoped API needs: the authorization
// lookups the gates perform, and the reads and writes the handlers behind them
// perform. *store.Store satisfies it.
//
// It is one interface rather than a field per resource so that the gates and
// the handlers they protect cannot be wired independently. A router that has
// the location handlers but not the session lookups would be a router serving
// a storage's tree to anyone who asked.
type APIStore interface {
	AuthStoreFull
	AdminStore
	LocationStore
	CategoryStore
	BatchStore
	ShoppingListStore
	ExpiryStore
	JobStore
	IdempotencyStore
	IngestStore
	ConsumeStore
	ProductStore
	ReorderStore
	AnalyticsStore
	GamificationStore
	StocktakeStore
	NotificationStore
	ExportStore
	BarcodeStore
	BarcodePromptStore
	InventoryStore
	MembershipStore
}

// Deps are the collaborators the router needs. StaticFS may be nil, in which
// case no static assets are served — useful in tests.
type Deps struct {
	DB       DBPinger
	Vision   VisionReporter
	StaticFS fs.FS
	// Errors serializes every error the API returns. When nil a production
	// writer is used, so a router built without one still cannot emit
	// debug_reason.
	Errors *ErrorWriter
	// Store backs the storage-scoped API. When nil those routes are not
	// registered at all — the failure mode of forgetting to pass one is
	// "the API is absent", never "the API is unguarded".
	Store APIStore
	// Matcher resolves free text to a product
	// (docs/specs/07-shopping-list-reconciliation.md). Required for the
	// shopping-list routes; when nil they are not registered.
	Matcher Matcher
	// Images produces image suggestions, and ImageCache serves the bytes they
	// point at. Both nil means the image routes are absent, which is the
	// honest state for a deployment with no provider configured.
	Images     ImageSuggester
	ImageCache ImageCache
	// InsecureCookies drops the Secure attribute from the session cookie. Set
	// it only for a dev deployment served over plain HTTP, where Secure would
	// stop the cookie working at all.
	//
	// Named for the exception rather than the rule, deliberately: a Deps built
	// without mentioning it gets Secure cookies. The failure mode of forgetting
	// this field has to be the safe one — the same reasoning that makes a nil
	// Errors writer default to production mode rather than dev.
	InsecureCookies bool
	// Ingester starts photo ingestion (docs/specs/06-vision-shelf-ingestion.md),
	// and Photos holds the photos behind review jobs. With no Ingester the
	// upload routes are absent; confirming and discarding existing jobs still
	// work.
	Ingester Ingester
	Photos   PhotoStore
	// ProductImages is permanent storage for product pictures taken from a
	// reviewed photo. Nil disables taking one — a confirm asking for a picture
	// is an internal error; one that does not is unaffected — and every stored
	// picture answers 404.
	ProductImages PhotoStore
	// Consumer starts consumption-photo ingestion
	// (docs/specs/09-consumption-logging.md), the same way Ingester starts
	// shelf and product ingestion. With no Consumer the upload route is
	// absent; confirming and discarding existing consumption jobs still work.
	Consumer Consumer
	// Backgrounds removes the background from a picture taken from a reviewed
	// photo, and Cutouts keeps the results while the review lasts
	// (docs/specs/09-consumption-logging.md). Either nil — GEMINI_IMAGE_MODEL
	// unset, or no usable upload volume — means no background removal: its
	// routes are absent and no job offers it.
	Backgrounds BackgroundRemover
	Cutouts     CutoutStore
	// AdminVision backs the admin settings routes and the /admin AI-model
	// banner. With no AdminVision those routes still register (settings has
	// no other prerequisite), but report model_unavailable / an empty model
	// list rather than panicking on a nil dependency.
	AdminVision AdminVisionChecker
	// Config is the running configuration, needed only to regenerate a
	// downloadable .env for GET /api/admin/settings/env-file. Nil disables
	// that one route; every other admin route is unaffected.
	Config *config.Config
	// Notifier delivers the expiry digest's test message
	// (docs/specs/17-expiry-notifications.md). Nil leaves reading and saving
	// notification settings working and makes the test route absent — the
	// scheduler that sends the real digests lives in cmd/inventory, not here.
	Notifier Notifier
	// Logger receives the one completion line per request
	// (docs/specs/18-operations-and-observability.md). Nil uses
	// slog.Default(), which in the running server is the JSON-to-stdout
	// logger cmd/inventory installs.
	Logger *slog.Logger
	// Version is the build's version string, reported by GET /healthz and
	// shown in the admin footer, so "what is the NAS actually running" is
	// answerable without SSH. Empty reports "dev", which is what an
	// unstamped build is.
	Version string
	// BarcodeDecoder reads a printed barcode out of an uploaded photograph
	// (docs/specs/20-barcode-recall.md's decode fallback). Nil uses the
	// package decoder in internal/barcode, which is what the running server
	// wants; a test substitutes one so the route's non-retention guarantee can
	// be asserted without depending on a real photo decoding.
	//
	// Unlike every other optional collaborator here, a nil value does not make
	// the route absent: there is always a decoder, because decoding is local
	// and has no configuration to be missing.
	BarcodeDecoder PhotoDecoder
}

// NewRouter builds the application's HTTP handler.
//
// Route groups follow docs/specs/04-backend-api-conventions.md: /healthz is
// unauthenticated because it is consumed by the compose healthcheck and by
// Traefik, and "/" serves the static frontend.
func NewRouter(d Deps) http.Handler {
	errs := d.Errors
	if errs == nil {
		// Defaulting to a production writer rather than panicking keeps a
		// misconfigured router safe: the failure mode of forgetting to pass
		// one must be "no reasons disclosed", never "all of them".
		errs = NewErrorWriter(false, nil)
	}

	r := chi.NewRouter()
	// Our own RequestLogger replaces chi's middleware.RequestID, rather than
	// stacking on top of it: two ids for one request is one id too many, and
	// only ours is a UUIDv7 that reaches the X-Request-Id header and every log
	// line of the request (docs/specs/18-operations-and-observability.md).
	// Nothing read chi's — middleware.GetReqID had no callers.
	//
	// It is registered **before** Recoverer, which makes it the outer of the
	// two, so a panic is already a 500 by the time the completion line is
	// written. See RequestLogger's own comment (requestlog.go).
	r.Use(RequestLogger(d.Logger))
	// TrustedRealIP, not chi's middleware.RealIP: the latter rewrites
	// RemoteAddr from X-Forwarded-For whoever the peer is, and the credential
	// rate limiter keys on the result (ratelimit.go,
	// docs/specs/14-account-self-service.md).
	r.Use(TrustedRealIP)
	r.Use(middleware.Recoverer)

	// chi's defaults answer with plain text ("404 page not found"), which is
	// neither the API's error shape nor something a client can switch on. Both
	// go through the one serializer instead, so there is exactly one error
	// format in the system.
	//
	// **Neither reason names the path.** A Failure's reason is written to the
	// log on every refusal and serialized as debug_reason in dev, so a reason
	// built from req.URL.Path echoes whatever was probed into the log file —
	// the very thing docs/specs/18-operations-and-observability.md keeps out
	// of the request line. The caller already knows the path it asked for, so
	// nothing is lost by leaving it out.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		errs.WriteError(w, req, NotFound(ReasonNoRouteMatch))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		errs.WriteError(w, req, &Failure{
			Status:  http.StatusMethodNotAllowed,
			Code:    "method_not_allowed",
			Message: "That method is not allowed here.",
			Reason:  ReasonMethodNotAllowed,
		})
	})

	r.Get("/healthz", HealthHandler(d.DB, d.Vision, d.Version))

	if d.Store != nil {
		// One sub-router carries the gate chain, and every storage-scoped route
		// is registered on it (docs/specs/04-backend-api-conventions.md). That
		// is what makes adding a route the same act as protecting it: there is
		// no way to hang a new /api/storages/... handler somewhere that skips
		// RequireStorageMember, because the pattern itself lives here.
		mw := NewMiddleware(d.Store, errs)
		// Idempotency sits after the gates in every group it is used in: keys
		// are per user, so it needs the session, and a request the gates refuse
		// must never be recorded (docs/specs/12-client-api-contract.md).
		idem := NewIdempotency(d.Store, errs)
		locations := NewLocationHandler(d.Store, errs)
		batches := NewBatchHandler(d.Store, errs)
		gamification := NewGamificationHandler(d.Store, errs)

		// The session lifecycle (docs/specs/03-auth-and-multi-tenancy.md).
		//
		// Login and pair are deliberately outside the session gate — they are
		// how a caller gets a session in the first place. Everything else here
		// sits behind RequireSession, including logout: revoking a session you
		// cannot prove you hold is not a thing to offer.
		// One credential limiter for login, pairing and password change
		// (docs/specs/14-account-self-service.md). Built here and handed to
		// all three handlers precisely so it is one counter: three limiters
		// would mean ten guesses each rather than ten in total, against
		// endpoints that are interchangeable to whoever is guessing.
		credentials := newRateLimiter(credentialFailuresPerWindow, credentialWindow)

		authHandler := NewAuthHandler(d.Store, errs, !d.InsecureCookies, credentials)
		devices := NewDeviceHandler(d.Store, errs, credentials)
		account := NewAccountHandler(d.Store, errs, credentials)

		r.Post("/api/auth/login", authHandler.Login)
		r.Post("/api/auth/pair", devices.Pair)

		r.Group(func(ar chi.Router) {
			ar.Use(mw.RequireSession)
			ar.Use(idem.Middleware)

			ar.Post("/api/auth/logout", authHandler.Logout)
			ar.Get("/api/auth/me", authHandler.Me)
			ar.Post("/api/auth/pairing-codes", devices.CreatePairingCode)
			ar.Get("/api/auth/devices", devices.ListDevices)
			ar.Delete("/api/auth/devices/{session_id}", devices.RevokeDevice)

			// Account self-service (docs/specs/14-account-self-service.md):
			// the caller's own credentials and display name. Session-gated
			// only — they act on the row the session names, so there is no id
			// in either request to scope or tamper with.
			ar.Post("/api/auth/password", account.ChangePassword)
			ar.Patch("/api/auth/me", account.UpdateMe)

			// The caller's own aggregate progress and preferences
			// (docs/specs/51-gamification-scoring.md) — session-scoped, not
			// storage-scoped, since they span every storage the caller belongs
			// to and are surfaced on the profile page, not any one dashboard.
			ar.Get("/api/me/progress", gamification.MeProgress)
			ar.Get("/api/me/preferences", gamification.MePreferences)
			ar.Put("/api/me/preferences", gamification.UpdateMePreferences)

			// The capture-time barcode offer's per-user preference
			// (docs/specs/20-barcode-recall.md). Session-scoped beside
			// /api/auth/password, not storage-scoped: the preference is the
			// person's, and it applies in every storage they belong to.
			barcodePrompt := NewBarcodePromptHandler(d.Store, errs)
			ar.Get("/api/auth/barcode-prompt", barcodePrompt.Get)
			ar.Patch("/api/auth/barcode-prompt", barcodePrompt.Patch)
			ar.Post("/api/auth/barcode-prompt/shown", barcodePrompt.Shown)

			// The instance-wide hot-cache list
			// (docs/specs/24-barcode-hot-cache.md): session-scoped, not
			// storage-scoped, since catalog_barcodes carries no storage
			// reference and every caller sees the same up to 500 rows
			// regardless of which storage they act in. A second
			// BarcodeHandler here rather than reaching into the storage
			// sub-router's one below — the two are registered on different
			// groups with different gate chains, and nothing but the
			// constructor is shared.
			hotBarcodes := NewBarcodeHandler(d.Store, d.BarcodeDecoder, d.ImageCache, d.ProductImages, errs)
			ar.Get("/api/barcodes/hot", hotBarcodes.Hot)

			// The picture behind a hot-cache card, on the same non-storage-
			// scoped terms as the list itself. Absent, like the storage-scoped
			// /images/{hash} above, when no image cache is configured.
			if d.ImageCache != nil {
				catalogImages := NewImageHandler(d.Images, d.ImageCache, errs)
				ar.Get("/api/catalog-images/{hash}", catalogImages.ServeCatalog)
			}
		})

		// The admin area: the HTML page and the JSON routes it posts to, on one
		// group with one gate chain (docs/specs/04-backend-api-conventions.md).
		// They are not two paths of different strength — a route added here for
		// either is behind RequireAdmin by the act of being added, and a
		// non-admin gets the same 404 from both as from a path that does not
		// exist at all.
		adminAPI := NewAdminHandler(d.Store, d.AdminVision, d.Config, errs)
		var imageModel admin.ImageModelChecker
		if d.Backgrounds != nil {
			imageModel = d.Backgrounds
		}
		adminPages, err := admin.New(d.Store, d.AdminVision, imageModel, d.Version,
			func(req *http.Request) (uuid.UUID, bool) {
				u, ok := UserFrom(req.Context())
				if !ok {
					return uuid.Nil, false
				}
				return u.ID, true
			},
			// Through the one serializer, and through the one store-error
			// mapper: an admin page reads the store like any handler, so a
			// mangled audit cursor must be the same 422 a JSON route would
			// give it rather than a 500 that blames the server for the
			// operator's edited URL.
			func(w http.ResponseWriter, req *http.Request, err error) {
				errs.WriteError(w, req, FromStoreError(err, "not found"))
			},
		)
		if err != nil {
			// The template is embedded in the binary, so this is a build defect
			// rather than a runtime condition — the template.Must situation.
			// Failing at startup is the point: the alternative is a server that
			// boots and 500s the first time an admin opens the page.
			panic(err)
		}

		r.Group(func(ad chi.Router) {
			ad.Use(mw.RequireSession)
			ad.Use(mw.RequireAdmin)
			ad.Use(idem.Middleware)

			ad.Get("/admin", adminPages.Page)

			ad.Get("/api/admin/users", adminAPI.ListUsers)
			ad.Post("/api/admin/users", adminAPI.CreateUser)
			ad.Delete("/api/admin/users/{id}", adminAPI.DeleteUser)
			ad.Post("/api/admin/users/{id}/password", adminAPI.ResetPassword)
			ad.Get("/api/admin/storages", adminAPI.ListStorages)
			ad.Post("/api/admin/storages", adminAPI.CreateStorage)
			ad.Delete("/api/admin/storages/{id}", adminAPI.DeleteStorage)
			ad.Get("/api/admin/storages/{id}/members", adminAPI.ListMembers)
			ad.Post("/api/admin/storages/{id}/members", adminAPI.AddMember)
			ad.Delete("/api/admin/storages/{id}/members/{user_id}", adminAPI.RemoveMember)

			ad.Get("/api/admin/settings", adminAPI.GetSettings)
			ad.Put("/api/admin/settings", adminAPI.PutSettings)
			if d.Config != nil {
				ad.Get("/api/admin/settings/env-file", adminAPI.EnvFile)
			}
			ad.Get("/api/admin/catalog", adminAPI.SearchCatalog)
			ad.Patch("/api/admin/catalog/{id}", adminAPI.PatchCatalog)
			ad.Delete("/api/admin/catalog/{id}", adminAPI.DeleteCatalog)

			// Moderation of a wrong global barcode mapping
			// (docs/specs/20-barcode-recall.md). Its own path rather than a
			// sub-route of /api/admin/catalog/{id}: catalog_barcodes is keyed
			// by the code, not by a catalog UUID, so {id} would not parse.
			ad.Delete("/api/admin/catalog-barcodes/{barcode}", adminAPI.DeleteCatalogBarcode)

			// The admin audit trail
			// (docs/specs/18-operations-and-observability.md). Server-rendered
			// and read-only, on this group like every other admin route: a
			// non-admin gets the same 404 for it as for a path that does not
			// exist, and there is no JSON counterpart anywhere.
			ad.Get("/admin/audit", adminPages.Audit)
		})

		r.Route("/api/storages/{storage_id}", func(sr chi.Router) {
			sr.Use(mw.RequireSession)
			sr.Use(mw.RequireStorageMember)
			sr.Use(idem.Middleware)

			sr.Get("/locations", locations.List)
			sr.Post("/locations", locations.Create)
			sr.Patch("/locations/{id}", locations.Update)
			sr.Delete("/locations/{id}", locations.Delete)

			sr.Patch("/inventory-batches/{id}", batches.Update)
			sr.Post("/inventory-batches/{id}/split", batches.Split)

			expiry := NewExpiryHandler(d.Store, errs)
			sr.Patch("/inventory-batches/{id}/expiry", expiry.PatchBatchExpiry)
			sr.Patch("/categories/{id}/shelf-life", expiry.PatchCategoryShelfLife)

			categories := NewCategoryHandler(d.Store, errs)
			sr.Get("/categories", categories.List)
			sr.Post("/categories", categories.Create)
			sr.Patch("/categories/{id}", categories.Update)
			sr.Delete("/categories/{id}", categories.Delete)

			// Background jobs (docs/specs/04-backend-api-conventions.md). The
			// endpoints that create them are the upload routes of spec 06.
			// Both nil or neither, so no route or response offers half of
			// background removal.
			backgrounds, cutouts := d.Backgrounds, d.Cutouts
			if backgrounds == nil || cutouts == nil {
				backgrounds, cutouts = nil, nil
			}

			jobsAPI := NewJobHandler(d.Store, d.Photos, reanalyzers(d), cutouts, backgrounds, errs)
			sr.Get("/jobs", jobsAPI.List)
			sr.Get("/jobs/{id}", jobsAPI.Get)
			sr.Get("/jobs/{id}/image", jobsAPI.Image)
			sr.Delete("/jobs/{id}", jobsAPI.Delete)
			// "Analyze again" (docs/specs/09-consumption-logging.md), for
			// every kind of job whose service is wired.
			sr.Post("/jobs/{id}/reanalyze", jobsAPI.Reanalyze)
			// "Discard all" (docs/specs/32-inbox-discard-all.md): the whole
			// inbox up to a server timestamp, in one request.
			sr.Delete("/jobs", jobsAPI.DiscardAll)

			// Photo ingestion (docs/specs/06-vision-shelf-ingestion.md).
			ingestAPI := NewIngestHandler(d.Ingester, d.Store, d.Photos, d.ProductImages, cutouts, backgrounds, errs)
			if d.Ingester != nil {
				sr.Post("/ingest/shelf-photos", ingestAPI.ShelfPhoto)
				sr.Post("/ingest/product-photos", ingestAPI.ProductPhoto)
			}
			sr.Post("/ingest/{job_id}/confirm", ingestAPI.Confirm)
			if backgrounds != nil {
				sr.Post("/ingest/{job_id}/cutouts", ingestAPI.Cutout)
				sr.Get("/ingest/{job_id}/cutouts/{cutout_id}", ingestAPI.CutoutImage)
			}

			// Product pictures cut from a reviewed photo, behind the same
			// membership gate as everything else here.
			productImages := NewProductImageHandler(d.Store, d.ProductImages, errs)
			sr.Get("/product-images/{name}", productImages.Serve)

			// Consumption logging (docs/specs/09-consumption-logging.md), the
			// same upload-then-confirm shape as ingestion above.
			consumeAPI := NewConsumeHandler(d.Consumer, d.Store, errs)
			if d.Consumer != nil {
				sr.Post("/consume/photos", consumeAPI.Upload)
			}
			sr.Post("/consume/photos/{job_id}/confirm", consumeAPI.Confirm)

			// Read-only product lookups consumption logging's manual-correction
			// and batch-picker need (docs/specs/09-consumption-logging.md), plus
			// the two narrow product-editing writes spec 52's "uncategorized"
			// and "imageless" quests need to be closeable at all.
			products := NewProductHandler(d.Store, d.ImageCache, d.ProductImages, errs)
			sr.Get("/products", products.List)
			sr.Get("/products/{product_id}/batches", products.Batches)
			sr.Patch("/products/{product_id}/category", products.SetCategory)
			sr.Patch("/products/{product_id}/image", products.SetImage)

			// Product maintenance (docs/specs/16-product-maintenance.md): the
			// detail read behind products.html, the full edit surface — which
			// is also the endpoint spec 08 noted as missing for
			// products.default_shelf_life_days — the duplicate merge, and the
			// delete. They join the group above rather than forming one of
			// their own: membership is the whole of their access control, like
			// every other route here.
			sr.Get("/products/{product_id}", products.Get)
			sr.Patch("/products/{product_id}", products.Update)
			sr.Post("/products/{product_id}/merge", products.Merge)
			sr.Delete("/products/{product_id}", products.Delete)

			if d.Matcher != nil {
				lists := NewShoppingListHandler(d.Store, d.Matcher, d.ImageCache, d.ProductImages, errs)
				sr.Post("/shopping-lists", lists.Create)
				sr.Get("/shopping-lists/{id}", lists.Get)
				sr.Post("/shopping-lists/{id}/items/{item_id}/rematch", lists.Rematch)
				sr.Post("/shopping-lists/{id}/items/{item_id}/resolve", lists.Resolve)
			}

			if d.Images != nil && d.ImageCache != nil {
				images := NewImageHandler(d.Images, d.ImageCache, errs)
				sr.Get("/image-suggestions", images.Suggest)
				sr.Get("/images/{hash}", images.Serve)
			}

			// Reorder dashboard and export (docs/specs/10-reorder-and-shopping-export.md).
			// The dashboard and export need only the store; the "Add item" flow
			// additionally needs the matcher, so those two routes follow the same
			// "absent collaborator, absent route" rule as the shopping-list group
			// above.
			reorder := NewReorderHandler(d.Store, d.Matcher, d.ImageCache, d.ProductImages, errs)
			sr.Get("/dashboard/reorder", reorder.Dashboard)
			sr.Get("/dashboard/reorder/export", reorder.Export)
			if d.Matcher != nil {
				sr.Post("/dashboard/reorder/items/match", reorder.Match)
				sr.Post("/dashboard/reorder/items", reorder.AddItem)
			}

			// Reporting & analytics (docs/specs/11-reporting-and-analytics.md),
			// sharing the dashboard page with the reorder widgets above.
			analytics := NewAnalyticsHandler(d.Store, errs)
			sr.Get("/dashboard/analytics", analytics.Dashboard)

			// Gamification (docs/specs/51-gamification-scoring.md,
			// docs/specs/52-gamification-quests-and-ui.md): this storage's
			// progress, its optional leaderboard, its flat any-member-may-change
			// toggles, this week's quests, and the caller's unlocked achievements.
			sr.Get("/progress", gamification.Progress)
			sr.Get("/progress/leaderboard", gamification.Leaderboard)
			sr.Get("/gamification/settings", gamification.StorageSettings)
			sr.Put("/gamification/settings", gamification.UpdateStorageSettings)
			sr.Get("/quests", gamification.Quests)
			sr.Get("/achievements", gamification.Achievements)

			// Stocktake and manual inventory correction
			// (docs/specs/13-stocktake-and-audit.md): found stock the system
			// never recorded, and the guided walk of one location. The
			// single-batch quantity fix rides the existing
			// PATCH /inventory-batches/{id} above rather than adding a route.
			stocktake := NewStocktakeHandler(d.Store, errs)
			sr.Post("/inventory-batches", stocktake.CreateBatch)
			sr.Get("/locations/{id}/stocktake", stocktake.Sheet)
			sr.Post("/locations/{id}/stocktake", stocktake.Confirm)

			// Expiry notifications (docs/specs/17-expiry-notifications.md):
			// one opt-in, per-storage digest configuration. Any member may
			// change it — rights inside a storage are flat — and the test
			// route is absent when no delivery service is wired, like every
			// other route whose collaborator is optional.
			notifications := NewNotificationHandler(d.Store, d.Notifier, errs)
			sr.Get("/notification-settings", notifications.Get)
			sr.Put("/notification-settings", notifications.Put)
			if d.Notifier != nil {
				sr.Post("/notification-settings/test", notifications.Test)
			}

			// Member export (docs/specs/15-backup-restore-and-export.md): one
			// storage as portable files. It is on this sub-router like
			// everything else, which is the whole of its access control — a
			// non-member gets the same 404 here as for a storage that does not
			// exist, and any member may export, since rights inside a storage
			// are flat.
			exports := NewExportHandler(d.Store, d.ProductImages, errs)
			sr.Get("/export", exports.Export)

			// Barcode recall (docs/specs/20-barcode-recall.md). Membership is
			// the whole of the access control, like everything else on this
			// sub-router: a non-member gets the same 404 for a scanned code as
			// for a storage that does not exist, and one storage's association
			// is invisible to another by the (storage_id, barcode) key alone.
			//
			// The log route is the only write in the scan flow, and it fires
			// on the confirm tap; the lookup above it is a read. Idempotency
			// rides the group's middleware like every other write here.
			barcodes := NewBarcodeHandler(d.Store, d.BarcodeDecoder, d.ImageCache, d.ProductImages, errs)
			sr.Get("/products/{product_id}/barcodes", barcodes.ListForProduct)
			sr.Post("/products/{product_id}/barcodes", barcodes.Associate)
			sr.Delete("/products/{product_id}/barcodes/{barcode}", barcodes.Delete)
			sr.Post("/barcodes/decode", barcodes.DecodePhoto)
			sr.Get("/barcodes/{code}", barcodes.Lookup)
			sr.Post("/barcodes/{code}/log", barcodes.Log)
			sr.Post("/barcodes/{code}/product", barcodes.AcceptCatalog)

			// The whole-inventory table (docs/specs/33-inventory-overview-table.md),
			// the read partner of the stocktake group's POST above on the same
			// collection.
			inventory := NewInventoryHandler(d.Store, errs)
			sr.Get("/inventory-batches", inventory.List)

			// The caller's own membership of this storage
			// (docs/specs/34-navigation-and-start-page.md): one personal,
			// per-storage display preference. On this sub-router like
			// everything else, which is the whole of its access control — a
			// non-member gets the identical 404 an unknown storage gets — and
			// with no user id in the path, so the only row it can reach is the
			// session's own. Distinct from PUT /api/me/preferences, which is
			// per user rather than per storage.
			membership := NewMembershipHandler(d.Store, errs)
			sr.Patch("/membership", membership.Patch)
		})

		// First-run guidance (docs/specs/29-first-run-admin-guidance.md): the
		// browser navigation routes, which are neither API nor admin area.
		//
		// The gate chain is the whole point of this group. /no-storages is
		// reached by every user who has no storage, which on a fresh
		// deployment is the bootstrap admin and on any deployment is every
		// person waiting to be added to one — so RequireAdmin would answer
		// 404 to precisely the callers it exists for, and the group sits
		// outside the admin group above for that reason rather than by
		// accident. It carries Idempotency like every other session-gated
		// group, which is a no-op here: the middleware only acts on writes,
		// and nothing but GET is routed into this group at all.
		navigation := NewNavigationHandler(d.Store, errs)
		r.Group(func(nr chi.Router) {
			nr.Use(noStore)
			nr.Use(mw.RequireSessionRedirect(LoginPage))
			nr.Use(idem.Middleware)

			// GET and nothing else. Every other verb falls through to the
			// static catch-all below and gets the 404 of a path that does not
			// exist — the same answer, from the same code, rather than a
			// second one written here to look like it.
			nr.Get("/no-storages", navigation.NoStorages)
		})
	}

	if d.StaticFS != nil {
		// Every method, and every miss through the one serializer.
		//
		// chi's r.NotFound only fires when no registered pattern matches a
		// request at all, and "/*" matches every path, so whatever this
		// catch-all answers for a missing path *is* the application's 404.
		// Two earlier versions got that wrong in turn:
		//
		//   - Registered for every method and handed straight to
		//     http.FileServer, it answered POST /api/auth/login — before that
		//     route existed — with the file server's plain-text
		//     "404 page not found" (verified live with curl).
		//   - Registered for GET and HEAD only, non-GET requests to unknown
		//     paths got chi's 405 instead, and GETs still reached the file
		//     server's plain text.
		//
		// Both broke the admin area's non-disclosure rule, not just the "one
		// error format" rule. A non-admin's GET /admin gets the serializer's
		// 404 from RequireAdmin; if GET /adminx got a different 404, or
		// POST /api/admin/nonexistent got a 405 where POST /api/admin/users
		// gets a 404, the difference would map out exactly which admin routes
		// exist (docs/specs/03-auth-and-multi-tenancy.md). staticHandler
		// answers every miss, for every method, with the same envelope the
		// gates use, so there is nothing to compare.
		//
		// The cost is that a wrong verb on a real route is a 404 rather than a
		// 405 when a static tree is mounted: chi falls through to this
		// catch-all before it would report the method mismatch. No client
		// here branches on that distinction, and a 405 that only appears on
		// routes that exist is the same leak in another form.
		r.HandleFunc("/*", staticHandler(d.StaticFS, errs))
	}
	return r
}

// staticHandler serves the frontend, answering anything that is not an asset
// with the serializer's 404.
func staticHandler(files fs.FS, errs *ErrorWriter) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(files))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			errs.WriteError(w, r, NotFound(ReasonNoRouteMatch))
			return
		}
		// The same name resolution http.FileServer performs, checked first so
		// a miss never reaches its plain-text 404. HEAD rides along with GET:
		// the file server writes headers only for it.
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "."
		}
		if _, err := fs.Stat(files, name); err != nil {
			errs.WriteError(w, r, NotFound(ReasonNoRouteMatch))
			return
		}
		// The service worker's own update check must never be satisfied from
		// a stale cached copy of the script itself
		// (docs/specs/05-frontend-pwa-foundations.md) — that would silently
		// defeat every fix this file's CACHE_VERSION bumps are supposed to
		// deliver, for the one file whose entire job is telling a client a
		// new version exists. no-cache (not no-store) still allows a
		// conditional GET against ETag/Last-Modified, so an unchanged
		// rebuild still gets a cheap 304 rather than a full re-download.
		if name == "sw.js" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	}
}
