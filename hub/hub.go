package hub

import (
	"log"
	"net/http"

	"github.com/spf13/viper"
	"github.com/getsentry/sentry-go"
	"github.com/newrelic/go-agent/v3/newrelic"
)

// Hub stores channels with clients currently subscribed and allows to dispatch updates.
type Hub struct {
	config             *viper.Viper
	transport          Transport
	server             *http.Server
	topicSelectorStore *TopicSelectorStore
	metrics            *Metrics
	newrelicApp        *newrelic.Application
}

// Stop stops disconnect all connected clients.
func (h *Hub) Stop() error {
	return h.transport.Close()
}

// NewHub creates a hub using the Viper configuration.
func NewHub(v *viper.Viper) (*Hub, error) {
	if err := ValidateConfig(v); err != nil {
		return nil, err
	}

	t, err := NewTransport(v)
	if err != nil {
		return nil, err
	}

	if dsn := v.GetString("sentry_dsn"); dsn != "" {
		err := sentry.Init(sentry.ClientOptions{
			// Either set your DSN here or set the SENTRY_DSN environment variable.
			Dsn: dsn,
			// Enable printing of SDK debug messages.
			// Useful when getting started or trying to figure something out.
			Debug: true,
		})
		if err != nil {
			log.Fatalf("sentry.Init: %s", err)
		}
	}

	var app *newrelic.Application
	if license := v.GetString("newrelic_license"); license != "" {
		log.Printf("Setting up NewRelic as %s with license %s", v.GetString("newrelic_name"), v.GetString("newrelic_license"))
		app, err = newrelic.NewApplication(
			newrelic.ConfigAppName(v.GetString("newrelic_name")),
			newrelic.ConfigLicense(license),
			newrelic.ConfigDistributedTracerEnabled(true),
		)

		if err != nil {
			log.Fatalf("newrelic.Init: %s", err)
		}
	}
	return NewHubWithTransport(v, t, app, NewTopicSelectorStore()), nil
}

// NewHubWithTransport creates a hub.
func NewHubWithTransport(v *viper.Viper, t Transport, nrApp *newrelic.Application, tss *TopicSelectorStore) *Hub {
	return &Hub{
		v,
		t,
		nil,
		tss,
		NewMetrics(),
		nrApp,
	}
}

// Start is an helper method to start the Mercure Hub.
func Start() {
	h, err := NewHub(viper.GetViper())
	if err != nil {
		log.Fatalln(err)
	}

	defer func() {
		if err = h.Stop(); err != nil {
			log.Fatalln(err)
		}
	}()

	h.Serve()
}
