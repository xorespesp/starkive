// `/api/check-deployment`: API that the setup tool calls before it records the dashboard address.
// It checks that the deployment at that address uses the DB repository and token from this setup.
//
//	GET ?nonce=<random value> -> { format: this server's DB repository format version, proof: `core.SignDeploymentNonce` }
//
// It is called without signing in.
// It answers from the environment variables alone, so it causes no GitHub request.
// The response alone reveals neither the token nor the repository.
// If the environment variable is missing or wrong, it answers with that 500 error (`{"error": ...}`).
// If the `nonce` has a wrong shape, it answers 400.

package handler

import (
	"net/http"

	"github.com/xorespesp/starkive/core"
)

// Response body of the deployment check
type checkDeploymentResult struct {
	Format int    `json:"format"` // This server's DB repository format version (`core.DBRepoFormat`). If it differs from the setup tool's, the dashboard runs a different version of the code
	Proof  string `json:"proof"`  // The `nonce` signed with the repository ID and token from the environment variable
}

// Signs the given `nonce` with this deployment's environment variable and answers with it and the format version.
func handleCheckDeploymentGet(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.LoadEnvConfig()
	if err != nil {
		return err
	}
	nonce := r.URL.Query().Get("nonce")
	if !core.IsValidDeploymentNonce(nonce) {
		return core.Errorf(http.StatusBadRequest, "Invalid nonce")
	}
	core.WriteJSON(w, http.StatusOK, checkDeploymentResult{Format: core.DBRepoFormat, Proof: core.SignDeploymentNonce(cfg, nonce)})
	return nil
}

// Entry point for `/api/check-deployment` requests.
// Vercel finds the exported function in this file and deploys it as one function.
// Accepts only GET. Other methods get 405.
func HandleCheckDeployment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		core.WriteMethodNotAllowed(w)
		return
	}
	core.Respond(w, r, handleCheckDeploymentGet)
}
