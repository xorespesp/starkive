// Tests for the things that define the DB repository format (`DBRepoFormat`).
package core

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Catches changes that may require raising the format version.
// It writes out the DB repository layout (path constants), the marker,
// and the JSON shape of the config file (`dbConfigFile`) and the data files (`StarItem`, `StarList`, `RepoMeta`), and pins their hash.
// When they change, the test fails.
// Then decide whether `DBRepoFormat` must go up, so an old dashboard or workflow doesn't read or write the new layout wrongly.
// If it doesn't need to go up, only update the hash.
// The contract with the workflow is guarded separately by `TestSyncWorkflowChangeIsReviewed` in the setup tool.
func TestDBRepoFormatChangeIsReviewed(t *testing.T) {
	shape := strings.Join([]string{
		MarkerDBPath, ConfigDBPath, StarItemsDBPath, StarListsDBPath, RepoMetaDBPath, SyncWorkflowPath,
		string(EncodeMarker()),
		describeJSONShape(reflect.TypeOf(dbConfigFile{})),
		describeJSONShape(reflect.TypeOf(StarItem{})),
		describeJSONShape(reflect.TypeOf(StarList{})),
		describeJSONShape(reflect.TypeOf(RepoMeta{})),
	}, "\n")
	const reviewed = "adef48b8a63a6a4872d4685fdfbf0a33e525db0d69cb561e2a012522f51dd0b4"
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(shape))); got != reviewed {
		t.Errorf("the DB repository format changed (sha256 %s). Decide whether core.DBRepoFormat must go up, then update the hash in this test:\n%s", got, shape)
	}
}

// Writes out, as text, the JSON shape of type `t`.
// For a struct, it writes the JSON tag and shape of each exported field, in field order (the field order is the key order in the file).
// For a pointer, slice or map, it writes the shape of the element.
// For a type that makes its own JSON (`json.Marshaler`, e.g. `time.Time`) and for everything else, it writes the type name
func describeJSONShape(t reflect.Type) string {
	marshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem() // Idiom to get an interface type: take the element of the type of a nil pointer
	if t.Implements(marshaler) || reflect.PointerTo(t).Implements(marshaler) {
		return t.String()
	}
	switch t.Kind() {
	case reflect.Pointer:
		return "*" + describeJSONShape(t.Elem())
	case reflect.Slice:
		return "[]" + describeJSONShape(t.Elem())
	case reflect.Map:
		return "map[" + describeJSONShape(t.Key()) + "]" + describeJSONShape(t.Elem())
	case reflect.Struct:
		fields := []string{}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if !field.IsExported() || field.Tag.Get("json") == "-" { // Fields that don't appear in JSON
				continue
			}
			fields = append(fields, fmt.Sprintf("%q:%s", field.Tag.Get("json"), describeJSONShape(field.Type)))
		}
		return "{" + strings.Join(fields, ",") + "}"
	}
	return t.Kind().String()
}
