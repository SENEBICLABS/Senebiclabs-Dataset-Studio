package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// MaxJSONBody is the request body limit for JSON endpoints (plan 15).
const MaxJSONBody = 1 << 20

// DecodeJSON reads exactly one JSON object from r's body into dst, rejecting
// unknown fields, trailing data and bodies over MaxJSONBody. On failure it
// writes a 400 or 413 problem response and returns false.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	body := http.MaxBytesReader(w, r.Body, MaxJSONBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if err == nil {
		if dec.Decode(&struct{}{}) != io.EOF {
			err = errors.New("unexpected data after the JSON object")
		}
	}
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		WriteProblem(w, r, Problem{Status: http.StatusRequestEntityTooLarge,
			Detail: fmt.Sprintf("request body exceeds %d bytes", MaxJSONBody)})
		return false
	}
	WriteProblem(w, r, Problem{Status: http.StatusBadRequest, Detail: "request body must be a single valid JSON object"})
	return false
}
