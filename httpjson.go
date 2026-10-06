package main

import (
	"encoding/json"
	"net/http"
)

// decodeJSON is the one place response bodies get decoded, shared by the
// qbittorrent and prowlarr clients — both otherwise duplicate the same
// three lines.
func decodeJSON(resp *http.Response, out any) error {
	return json.NewDecoder(resp.Body).Decode(out)
}
