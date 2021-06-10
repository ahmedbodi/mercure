package mercure

import (
	"io"
	"net/http"
	"strconv"

	"go.uber.org/zap"
)

// PublishHandler allows publisher to broadcast updates to all subscribers.
func (h *Hub) PublishHandler(w http.ResponseWriter, r *http.Request) {
	h.logger.Info("Got A new publish", zap.String("remote_addr", r.RemoteAddr), zap.String("Method", r.Method))
	var claims *claims
	if h.publisherJWT != nil {
		var err error
		claims, err = authorize(r, h.publisherJWT, h.publishOrigins)
		if err != nil || claims == nil || claims.Mercure.Publish == nil {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			h.logger.Info("Topic selectors not matched, not provided or authorization error", zap.String("remote_addr", r.RemoteAddr), zap.Error(err))

			return
		}
	}

	h.logger.Info("Parsing Form")
	if r.ParseForm() != nil {
		h.logger.Info("Error Parsing Form")
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)

		return
	}

	h.logger.Info("Checking Topics")
	topics := r.PostForm["topic"]
	if len(topics) == 0 {
		h.logger.Info("Topics Missing")
		http.Error(w, "Missing \"topic\" parameter", http.StatusBadRequest)

		return
	}

	h.logger.Info("Checking for retry parameter")
	var retry uint64
	if retryString := r.PostForm.Get("retry"); retryString != "" {
		h.logger.Info("Validating retry parameter")
		var err error
		if retry, err = strconv.ParseUint(retryString, 10, 64); err != nil {
			h.logger.Info("Invalid retry parameter")
			http.Error(w, "Invalid \"retry\" parameter", http.StatusBadRequest)

			return
		}
	}

	h.logger.Info("Checking if update is private and can be dispatched")
	private := len(r.PostForm["private"]) != 0
	if private && !canDispatch(h.topicSelectorStore, topics, claims.Mercure.Publish) {
		h.logger.Info("Permissions not acquired for private publishing")
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)

		return
	}

	h.logger.Info("Creating Update Object")
	u := &Update{
		Topics:  topics,
		Private: private,
		Debug:   h.debug,
		Event:   Event{r.PostForm.Get("data"), r.PostForm.Get("id"), r.PostForm.Get("type"), retry},
	}

	h.logger.Info("Dispatching Update")
	// Broadcast the update
	if err := h.transport.Dispatch(u); err != nil {
		panic(err)
	}

	h.logger.Info("Writing Update ID to Response")
	_, err := io.WriteString(w, u.ID)
	if err != nil {
		h.logger.Info("Error Writing Update ID to response", zap.Error(err))
	}
	h.logger.Info("Update published", zap.Object("update", u), zap.String("remote_addr", r.RemoteAddr))
	h.metrics.UpdatePublished(u)
}
