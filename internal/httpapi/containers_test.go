package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// The wire contract of docs/specs/39-batch-containers.md: container_label and
// container_type on the batch PATCH, container_disposition on the split, and the
// one endpoint that addresses a container by its own id.
//
// These tests are about what the *handler* decides — which patch shape reaches
// the store, which bodies are refused before a transaction opens, which status
// each store error becomes. What the store then does with a valid patch is
// internal/store/containers_test.go's subject, against a real database.

// TestPatchBatchDistinguishesAbsentFromNullContainerLabel is the one that fails
// silently if container_label is decoded into a *string: absent and null both
// arrive as nil, and "take this batch out of its container" quietly becomes
// "leave it alone" — a detach that answers 200 and changes nothing.
func TestPatchBatchDistinguishesAbsentFromNullContainerLabel(t *testing.T) {
	t.Parallel()

	t.Run("absent leaves the container alone", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(),
			`{"quantity":3}`)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.False(t, f.batches.lastPatch.SetContainerLabel,
			"a patch that never named container_label must not touch the container")
		assert.False(t, f.batches.lastPatch.SetContainerType)
	})

	t.Run("null detaches", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(),
			`{"container_label":null}`)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.True(t, f.batches.lastPatch.SetContainerLabel,
			"an explicit null is a request to detach, not an absent field")
		assert.Nil(t, f.batches.lastPatch.ContainerLabel)
	})

	t.Run("a label is passed through trimmed", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(),
			`{"container_label":"  24-pack box  "}`)

		require.Equal(t, http.StatusOK, rec.Code)
		require.NotNil(t, f.batches.lastPatch.ContainerLabel)
		assert.Equal(t, "24-pack box", *f.batches.lastPatch.ContainerLabel)
	})
}

func TestPatchBatchValidatesTheContainerFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{"empty label", `{"container_label":""}`, "container_label"},
		{"whitespace-only label", `{"container_label":"   "}`, "container_label"},
		{"label over 255 characters", `{"container_label":"` + strings.Repeat("b", 256) + `"}`, "container_label"},
		{"label of the wrong type", `{"container_label":7}`, "container_label"},
		{"empty type", `{"container_type":""}`, "container_type"},
		{"type of the wrong type", `{"container_type":[]}`, "container_type"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(), tc.body)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.NotEmpty(t, errorFields(t, rec)[tc.wantField])
		})
	}
}

// TestPatchBatchAcceptsAContainerFieldAlone covers the regression the empty-patch
// guard invites: it used to name two fields, and a PATCH carrying only a
// container field has to be a real patch rather than "you named nothing".
func TestPatchBatchAcceptsAContainerFieldAlone(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(),
		`{"container_type":"box"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, f.batches.lastPatch.SetContainerType)
	require.NotNil(t, f.batches.lastPatch.ContainerType)
	assert.Equal(t, "box", *f.batches.lastPatch.ContainerType)
	assert.Nil(t, f.batches.lastPatch.Quantity)
	assert.Nil(t, f.batches.lastPatch.LocationID)
}

// TestPatchBatchNamingNoFieldStillNamesTheContainerFields keeps the refusal
// honest: the 422 has to tell a client which fields it could have sent, and the
// two container ones are now among them.
func TestPatchBatchNamingNoFieldStillNamesTheContainerFields(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(), `{}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	fields := errorFields(t, rec)
	assert.NotEmpty(t, fields["container_label"])
	assert.NotEmpty(t, fields["container_type"])
}

// TestPatchBatchTypeWithoutAContainerIs422 is the store's decision, not the
// handler's — only the transaction knows whether the batch already has a
// container — so what this pins is that ErrValidation from that path reaches the
// wire as a 422 and not a 404.
func TestPatchBatchTypeWithoutAContainerIs422(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.batches.moveErr = store.ErrValidation

	rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(),
		`{"container_type":"box"}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "validation_failed", errorCode(t, rec))
}

func TestBatchResponseCarriesTheContainer(t *testing.T) {
	t.Parallel()

	containerID := uuid.New()
	label := "24-pack box"
	kind := "box"

	t.Run("a batch in a container", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		f.batches.moved = &store.Batch{
			ID: uuid.New(), LocationID: uuid.New(), Quantity: 22,
			ContainerID: &containerID, ContainerLabel: &label, ContainerType: &kind,
		}

		rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(),
			`{"container_label":"24-pack box"}`)

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			ContainerID    *uuid.UUID `json:"container_id"`
			ContainerLabel *string    `json:"container_label"`
			ContainerType  *string    `json:"container_type"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.NotNil(t, body.ContainerID)
		assert.Equal(t, containerID, *body.ContainerID)
		require.NotNil(t, body.ContainerLabel)
		assert.Equal(t, label, *body.ContainerLabel,
			"the label is what the UI shows; an id alone would need a second request")
		require.NotNil(t, body.ContainerType)
		assert.Equal(t, kind, *body.ContainerType)
	})

	// The keys are present-and-null rather than absent, so a client can tell
	// "this batch is in nothing" from "this response does not carry containers".
	t.Run("a batch in nothing still carries the keys", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t)
		f.batches.moved = &store.Batch{ID: uuid.New(), LocationID: uuid.New(), Quantity: 2}

		rec := f.do(http.MethodPatch, f.base()+"/inventory-batches/"+uuid.New().String(),
			`{"location_id":"`+uuid.New().String()+`"}`)

		require.Equal(t, http.StatusOK, rec.Code)
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
		for _, key := range []string{"container_id", "container_label", "container_type"} {
			value, ok := raw[key]
			require.True(t, ok, "%s must be serialized even when there is no container", key)
			assert.JSONEq(t, "null", string(value))
		}
	})
}

func TestSplitPassesTheContainerDisposition(t *testing.T) {
	t.Parallel()

	for _, disposition := range []string{"source", "target", "both", "neither", "destroy"} {
		t.Run(disposition, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPost, f.base()+"/inventory-batches/"+uuid.New().String()+"/split",
				`{"quantity":2,"target_location_id":"`+uuid.New().String()+`","container_disposition":"`+disposition+`"}`)

			require.Equal(t, http.StatusCreated, rec.Code)
			assert.Equal(t, store.ContainerDisposition(disposition),
				f.batches.lastSplit.ContainerDisposition)
		})
	}
}

// TestSplitWithoutADispositionLeavesTheDefaultToTheStore keeps the default in
// one place. The handler forwarding an empty disposition rather than spelling
// "source" itself is what stops the default drifting between the two layers.
func TestSplitWithoutADispositionLeavesTheDefaultToTheStore(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/inventory-batches/"+uuid.New().String()+"/split",
		`{"quantity":2,"target_location_id":"`+uuid.New().String()+`"}`)

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, store.ContainerDisposition(""), f.batches.lastSplit.ContainerDisposition)
}

func TestSplitRejectsAnUnknownContainerDisposition(t *testing.T) {
	t.Parallel()

	for _, value := range []string{`"keep"`, `"SOURCE"`, `""`, `5`} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPost, f.base()+"/inventory-batches/"+uuid.New().String()+"/split",
				`{"quantity":2,"target_location_id":"`+uuid.New().String()+`","container_disposition":`+value+`}`)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Empty(t, f.batches.lastSplit.Quantity,
				"an unknown disposition must be refused before the store is asked to split")
		})
	}
}

func TestDestroyContainerAnswers204AndPassesTheValidatedStorage(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	containerID := uuid.New()

	rec := f.do(http.MethodPost, f.base()+"/containers/"+containerID.String()+"/destroy", "")

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String(), "there is nothing left to describe")
	assert.Equal(t, f.storageID, f.batches.lastDestroyedIn,
		"the storage reaching the store is the one the middleware validated, never one parsed from the URL")
	assert.Equal(t, []uuid.UUID{containerID}, f.batches.destroyedIDs)
}

// TestDestroyContainerRefusalsAreIndistinguishable is the non-enumeration rule
// of docs/specs/03-auth-and-multi-tenancy.md applied to the one container id
// that arrives straight from the URL. Unknown, foreign-storage and
// already-destroyed all reach the handler as ErrNotFound, and all three have to
// leave it as the same 404 — not a 403, which would confirm the id names a real
// container somewhere, and not a 409, which would confirm it named one here.
func TestDestroyContainerRefusalsAreIndistinguishable(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.batches.destroyErr = store.ErrNotFound

	known := f.do(http.MethodPost, f.base()+"/containers/"+uuid.New().String()+"/destroy", "")
	malformed := f.do(http.MethodPost, f.base()+"/containers/not-a-uuid/destroy", "")

	require.Equal(t, http.StatusNotFound, known.Code)
	require.Equal(t, http.StatusNotFound, malformed.Code)
	assert.Equal(t, "not_found", errorCode(t, known))
	assert.Equal(t, known.Body.String(), malformed.Body.String(),
		"a well-formed id nobody may see and a malformed one must read identically")
}

// TestDestroyContainerOutsideAMembershipIs404 covers the route itself, not the
// store: a non-member gets the same 404 for it as for a path that does not
// exist, which is what keeps the storage id in the URL from being an oracle.
func TestDestroyContainerOutsideAMembershipIs404(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	foreign := "/api/storages/" + uuid.New().String()

	rec := f.do(http.MethodPost, foreign+"/containers/"+uuid.New().String()+"/destroy", "")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, f.batches.destroyedIDs, "the store must never be reached")
}

func TestDestroyContainerNeedsASession(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.anonymous(http.MethodPost, f.base()+"/containers/"+uuid.New().String()+"/destroy", "")

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, f.batches.destroyedIDs)
}
