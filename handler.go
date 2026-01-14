package mercure

import (
	"io/fs"
	"log/slog"
	"context"
	"errors"
	"fmt"
	newrelic "github.com/newrelic/go-agent"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/rs/cors"
	"github.com/unrolled/secure"
)

const (
	defaultHubURL  = "/.well-known/mercure"
	defaultUIURL   = defaultHubURL + "/ui/"
	defaultDemoURL = defaultUIURL + "demo/"
)

func (h *Hub) initHandler() {
	router := mux.NewRouter()
	router.UseEncodedPath()
	router.SkipClean(true)

	csp := "default-src 'self'"

	if h.demo {
		router.PathPrefix(defaultDemoURL).HandlerFunc(h.Demo).Methods(http.MethodGet, http.MethodHead)
	}

	if h.ui {
		public, err := fs.Sub(uiContent, "public")
		if err != nil {
			panic(err)
		}

		router.PathPrefix(defaultUIURL).Handler(http.StripPrefix(defaultUIURL, http.FileServer(http.FS(public))))

		csp += " mercure.rocks cdn.jsdelivr.net"
	}

	h.registerSubscriptionHandlers(router)

	if h.newrelicApp != nil {
		h.logger.Info("Wrapping HTTP Handlers with NewRelic")

		if h.subscriberJWTKeyFunc != nil || h.anonymous {
			router.HandleFunc(newrelic.WrapHandleFunc(h.newrelicApp, defaultHubURL, h.SubscribeHandler)).Methods(http.MethodGet, http.MethodHead)
		}

		if h.publisherJWTKeyFunc != nil {
			router.HandleFunc(newrelic.WrapHandleFunc(h.newrelicApp, defaultHubURL, h.PublishHandler)).Methods(http.MethodPost)
		}
	} else {
		h.logger.Info("Not Wrapping HTTP Handlers with NewRelic")
		
		if h.subscriberJWTKeyFunc != nil || h.anonymous {
			router.HandleFunc(defaultHubURL, h.SubscribeHandler).Methods(http.MethodGet, http.MethodHead)
		}

		if h.publisherJWTKeyFunc != nil {
			router.HandleFunc(defaultHubURL, h.PublishHandler).Methods(http.MethodPost)
		}
	}

	secureMiddleware := secure.New(secure.Options{
		IsDevelopment:         h.debug,
		AllowedHosts:          h.allowedHosts,
		FrameDeny:             true,
		ContentTypeNosniff:    true,
		BrowserXssFilter:      true,
		ContentSecurityPolicy: csp,
	})

	if len(h.corsOrigins) == 0 {
		h.handler = secureMiddleware.Handler(router)

		return
	}

	h.handler = secureMiddleware.Handler(
		cors.New(cors.Options{
			AllowedOrigins:   h.corsOrigins,
			AllowCredentials: true,
			AllowedHeaders:   []string{"authorization", "cache-control", "last-event-id"},
			Debug:            h.debug,
		}).Handler(router),
	)
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.ServeHTTP(w, r)
}

func (h *Hub) registerSubscriptionHandlers(r *mux.Router) {
	if !h.subscriptions {
		return
	}

	if _, ok := h.transport.(TransportSubscribers); !ok {
		if h.logger.Enabled(h.ctx, slog.LevelError) {
			h.logger.LogAttrs(h.ctx, slog.LevelError, "The current transport doesn't support subscriptions. Subscription API disabled.")
		}

		return
	}

	r.UseEncodedPath()
	r.SkipClean(true)

	if h.newrelicApp != nil {
		h.logger.Info("Wrapping HTTP Handlers with NewRelic")
		router.HandleFunc(newrelic.WrapHandleFunc(h.newrelicApp, subscriptionURL, h.SubscriptionHandler)).Methods(http.MethodGet)
		router.HandleFunc(newrelic.WrapHandleFunc(h.newrelicApp, subscriptionsForTopicURL, h.SubscriptionsHandler)).Methods(http.MethodGet)
		router.HandleFunc(newrelic.WrapHandleFunc(h.newrelicApp, subscriptionsURL, h.SubscriptionsHandler)).Methods(http.MethodGet)
	} else {
		h.logger.Info("Not Wrapping HTTP Handlers with NewRelic")
		r.HandleFunc(subscriptionURL, h.SubscriptionHandler).Methods(http.MethodGet)
		r.HandleFunc(subscriptionsForTopicURL, h.SubscriptionsHandler).Methods(http.MethodGet)
		r.HandleFunc(subscriptionsURL, h.SubscriptionsHandler).Methods(http.MethodGet)
	}
}
