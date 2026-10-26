// `/api/archive-star-lists`: API that reads and changes the star list definitions (`db/star-lists.json`).
//
//	GET              -> The raw star list file (a JSON array, one star list per line). `[]` if the file doesn't exist yet
//	POST             { name, description } -> The new star list. The server assigns the ID (`core.NewStarListID`)
//	PATCH  ?id=<id>  { name, description } -> The changed star list
//	DELETE ?id=<id>  -> { ok: true }. The repositories (items) stay in the archive. Only their membership in that star list is removed
//
// Every write reads the data it needs at one commit, applies the change, and writes it as one commit
// (`core.UpdateStarLists`, or `core.UpdateStarCatalog` when the items change too).

package handler

import (
	"net/http"
	"slices"
	"strings"

	"github.com/xorespesp/starkive/core"
)

// Reads a star list name and description from the request body, trims spaces at both ends, and checks the length.
//
// Returns:
//   - The trimmed name and description.
//     A 400 `HTTPError` if the body isn't JSON, the name is empty, or the name or description is too long
func readStarListBody(r *http.Request) (name string, description string, err error) {
	var body struct {
		Name        string `json:"name"`        // Star list name. Required
		Description string `json:"description"` // Description. May be empty
	}
	if err := core.ReadAPIRequestJSON(r, &body); err != nil {
		return "", "", err
	}
	name, description = strings.TrimSpace(body.Name), strings.TrimSpace(body.Description)
	if err := core.CheckTextLength("Name", name, true, core.MaxStarListNameLength); err != nil {
		return "", "", err
	}
	if err := core.CheckTextLength("Description", description, false, core.MaxStarListDescriptionLength); err != nil {
		return "", "", err
	}
	return name, description, nil
}

// Checks whether another star list with the same name already exists.
// `core.IsSameStarListName` decides whether names are the same (it ignores leading and trailing spaces, Unicode normalization (NFC) and letter case).
//
// Args:
//   - `starLists`: The star lists in the archive now
//   - `name`: The name to use
//   - `exceptID`: ID of a star list to leave out of the comparison (itself, when renaming). 0 when creating
//
// Returns:
//   - A 409 `HTTPError` if the name is taken
//
// NOTE: The sidebar on the page tells star lists apart only by name.
// If two have the same name, the user can't tell them apart.
func checkUniqueStarListName(starLists []core.StarList, name string, exceptID int64) error {
	for _, l := range starLists {
		if l.ID != exceptID && core.IsSameStarListName(l.Name, name) {
			return core.Errorf(http.StatusConflict, "A list named %q already exists", name)
		}
	}
	return nil
}

// Returns the raw star list file to a signed-in user.
// Answers 401 if not signed in.
// Answers 409 if the DB repository can't be used (not set up yet, a foreign repository, or a different major format version).
func handleArchiveStarListsGet(w http.ResponseWriter, r *http.Request) error {
	dbRepo, err := core.Authorize(r)
	if err != nil {
		return err
	}
	return core.WriteAPIDBFileResponse(w, r, dbRepo, core.StarListsDBPath)
}

// Creates a star list and answers with it as JSON.
// Answers 400 if the name or description is wrong, and 409 if the name is already taken.
func handleArchiveStarListsPost(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	dbRepo, err := core.Authorize(r)
	if err != nil {
		return err
	}
	name, description, err := readStarListBody(r)
	if err != nil {
		return err
	}
	var created core.StarList
	err = core.UpdateStarLists(ctx, dbRepo, func(starLists *[]core.StarList) (core.CommitMessage, error) {
		if err := checkUniqueStarListName(*starLists, name, 0); err != nil {
			return core.CommitMessage{}, err
		}
		created = core.StarList{ID: core.NewStarListID(*starLists), Name: name, Description: description} // If called again, it is set again with a new ID
		*starLists = append(*starLists, created)
		return core.NewCommitMessage("user(star-lists)", "create "+name), nil
	})
	if err != nil {
		return err
	}
	core.WriteAPIJSONResponse(w, http.StatusOK, created)
	return nil
}

// Changes the name and description of one star list and answers with it as JSON.
// Answers 400 if `id` isn't a number or the name or description is wrong.
// Answers 404 if the star list doesn't exist, and 409 if another star list has the same name.
func handleArchiveStarListsPatch(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	dbRepo, err := core.Authorize(r)
	if err != nil {
		return err
	}
	id, err := core.ParseAPIRequestID(r)
	if err != nil {
		return err
	}
	name, description, err := readStarListBody(r)
	if err != nil {
		return err
	}
	var updated core.StarList
	err = core.UpdateStarLists(ctx, dbRepo, func(starLists *[]core.StarList) (core.CommitMessage, error) {
		i := slices.IndexFunc(*starLists, func(l core.StarList) bool { return l.ID == id })
		if i < 0 {
			return core.CommitMessage{}, core.Errorf(http.StatusNotFound, "List not found")
		}
		if err := checkUniqueStarListName(*starLists, name, id); err != nil {
			return core.CommitMessage{}, err
		}
		(*starLists)[i].Name, (*starLists)[i].Description = name, description
		updated = (*starLists)[i]
		return core.NewCommitMessage("user(star-lists)", "edit "+name), nil
	})
	if err != nil {
		return err
	}
	core.WriteAPIJSONResponse(w, http.StatusOK, updated)
	return nil
}

// Deletes one star list and answers `{ ok: true }`.
// It also removes the membership from the items that were in that star list.
// The repositories (items) stay in the archive.
// Answers 400 if `id` isn't a number, and 404 if the star list doesn't exist.
//
// NOTE: The star list definitions (`db/star-lists.json`) and the item membership (the archive item shards) change together in one commit.
// So no in-between state, such as "an item that points to a deleted star list", is left in the repository.
func handleArchiveStarListsDelete(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	dbRepo, err := core.Authorize(r)
	if err != nil {
		return err
	}
	id, err := core.ParseAPIRequestID(r)
	if err != nil {
		return err
	}
	err = core.UpdateStarCatalog(ctx, dbRepo, func(tx *core.StarCatalogTransaction) (core.CommitMessage, error) {
		i := slices.IndexFunc(tx.StarLists, func(l core.StarList) bool { return l.ID == id })
		if i < 0 {
			return core.CommitMessage{}, core.Errorf(http.StatusNotFound, "List not found")
		}
		name := tx.StarLists[i].Name // `slices.Delete` shifts the later items into place, so the name is taken out before deleting
		tx.StarLists = slices.Delete(tx.StarLists, i, i+1)
		members := 0 // Number of repositories that were in this star list. Written in the commit body
		// Removes the membership from the items in the same commit that deletes the definition
		for j := range tx.StarItems {
			if slices.Contains(tx.StarItems[j].StarListIDs, id) {
				members++
				tx.StarItems[j].StarListIDs = slices.DeleteFunc(tx.StarItems[j].StarListIDs, func(x int64) bool { return x == id })
			}
		}
		removed := "" // An empty body section is left out of the commit message
		if members > 0 {
			removed = "Removed from " + core.FormatCount(members, "repo")
		}
		return core.NewCommitMessage("user(star-lists)", "delete "+name, removed), nil
	})
	if err != nil {
		return err
	}
	core.WriteAPIJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// Entry point for `/api/archive-star-lists` requests. It sends each method to its handler.
// Vercel finds the exported function in this file and deploys it as one function.
// `core.ServeAPIRequest` turns an error from a handler into a JSON error response.
// Other methods get 405.
func HandleArchiveStarLists(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		core.ServeAPIRequest(w, r, handleArchiveStarListsGet)
	case http.MethodPost:
		core.ServeAPIRequest(w, r, handleArchiveStarListsPost)
	case http.MethodPatch:
		core.ServeAPIRequest(w, r, handleArchiveStarListsPatch)
	case http.MethodDelete:
		core.ServeAPIRequest(w, r, handleArchiveStarListsDelete)
	default:
		core.WriteAPIMethodNotAllowed(w)
	}
}
