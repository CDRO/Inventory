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
//     404, byte for byte.
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
	r.Use(middleware.RequestID)
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
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		errs.WriteError(w, req, NotFound("no route matches "+req.URL.Path))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		errs.WriteError(w, req, &Failure{
			Status:  http.StatusMethodNotAllowed,
			Code:    "method_not_allowed",
			Message: "That method is not allowed here.",
			Reason:  req.Method + " on " + req.URL.Path,
		})
	})

	r.Get("/healthz", HealthHandler(d.DB, d.Vision))

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
		adminPages, err := admin.New(d.Store, d.AdminVision, imageModel,
			func(req *http.Request) (uuid.UUID, bool) {
				u, ok := UserFrom(req.Context())
				if !ok {
					return uuid.Nil, false
				}
				return u.ID, true
			},
			func(w http.ResponseWriter, req *http.Request, err error) {
				errs.WriteError(w, req, Internal(err))
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
			errs.WriteError(w, r, NotFound("no route matches "+r.Method+" "+r.URL.Path))
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
			errs.WriteError(w, r, NotFound("no route matches "+r.URL.Path))
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
