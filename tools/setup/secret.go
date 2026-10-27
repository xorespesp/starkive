// Writing Actions secrets in the DB repository.
// GitHub accepts a secret value only after it is encrypted with the repository's public key.
// So reading the public key and the encryption (sealed box) are here too.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"

	"golang.org/x/crypto/nacl/box"

	"github.com/xorespesp/starkive/core"
)

// Public key for the Actions secrets of the DB repository
type secretsPublicKey struct {
	KeyID string `json:"key_id"` // ID of the public key. Sent with a secret to say which key encrypted it
	Key   string `json:"key"`    // 32-byte Curve25519 public key in base64
}

// Reads the public key for the Actions secrets of the DB repository.
//
// NOTE: This request needs the Secrets read permission (measured).
// So if it succeeds, the permission is checked too.
// Without the permission, it returns GitHub's 403 error, with the needed permission added to its message (`core.CheckGitHubResponse`).
func readSecretsPublicKey(ctx context.Context, dbRepo *core.DBRepo) (secretsPublicKey, error) {
	res, err := core.CallGitHubAPI(ctx, dbRepo.EnvConfig(), dbRepo.BuildAPIPath("/actions/secrets/public-key"), core.GitHubAPIRequest{})
	if err != nil {
		return secretsPublicKey{}, err
	}
	defer res.Body.Close()
	if err := core.CheckGitHubResponse(res); err != nil {
		return secretsPublicKey{}, err
	}
	var key secretsPublicKey
	if err := json.NewDecoder(res.Body).Decode(&key); err != nil {
		return secretsPublicKey{}, err
	}
	return key, nil
}

// Writes one Actions secret to the DB repository.
// If a secret with the same name exists, it is replaced.
//
// Args:
//   - `key`: The repository public key from `readSecretsPublicKey`
//   - `name`: Secret name (e.g. `STARKIVE_SYNC_KEY`)
//   - `value`: The plain value. It is encrypted with the public key before sending
//
// NOTE: GitHub accepts the value only as a sealed box encrypted with the repository public key (docs).
// A value written this way comes back unchanged in a workflow run (measured).
// A secret value can't be read back through the API.
// So whether it went in correctly shows only when a workflow actually uses it.
func writeSecret(ctx context.Context, dbRepo *core.DBRepo, key secretsPublicKey, name string, value string) error {
	sealed, err := sealSecret(key.Key, value)
	if err != nil {
		return err
	}
	body := map[string]string{"encrypted_value": sealed, "key_id": key.KeyID}
	res, err := core.CallGitHubAPI(ctx, dbRepo.EnvConfig(), dbRepo.BuildAPIPath("/actions/secrets/")+name, core.GitHubAPIRequest{Method: http.MethodPut, Body: body})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return core.CheckGitHubResponse(res)
}

// Encrypts a value as a sealed box with the repository public key (the same format as libsodium's `crypto_box_seal`).
// A sealed box is encrypted with only the receiver's public key, without a sender key.
// Only the receiver's private key (which GitHub holds) can open it.
//
// Args:
//   - `publicKeyBase64`: 32-byte public key in base64 (`secretsPublicKey.Key`)
//   - `value`: The plain text to encrypt
//
// Returns:
//   - The ciphertext in base64. An error if the public key can't be decoded or isn't 32 bytes
func sealSecret(publicKeyBase64 string, value string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(publicKeyBase64)
	if err != nil || len(raw) != 32 {
		return "", errors.New("GitHub returned an unusable secrets public key")
	}
	var recipient [32]byte // `box` takes a fixed 32-byte array, not a slice
	copy(recipient[:], raw)
	sealed, err := box.SealAnonymous(nil, []byte(value), &recipient, rand.Reader)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}
