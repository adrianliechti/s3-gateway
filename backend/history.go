package backend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// NativeHistory exposes an immutable provider version of a published object.
// References are virtual backend keys, not additional objects. Empty means the
// provider has no immutable version (for example, Azure versioning is disabled).
// Head/Get/Delete must accept references returned by NativeVersion.
type NativeHistory interface {
	NativeVersion(context.Context, string, string) (string, error)
}

const nativeVersionPrefix = InternalPrefix + "native-version/"

// HistoryReferences decodes just the reachability information shared by
// version indexes and publication journals, without coupling adapters to managed.
func HistoryReferences(raw []byte) (key string, refs []string, err error) {
	var state struct {
		Key      string
		Versions []struct{ Data string }
		Sources  []ComposeSource
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		return "", nil, err
	}
	for _, v := range state.Versions {
		if v.Data != "" {
			refs = append(refs, v.Data)
		}
	}
	for _, s := range state.Sources {
		refs = append(refs, s.Key)
	}
	return state.Key, refs, nil
}

func VersionReference(key, version string) string {
	raw, _ := json.Marshal([2]string{key, version})
	return nativeVersionPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func ParseVersionReference(ref string) (key, version string, ok bool, err error) {
	if !strings.HasPrefix(ref, nativeVersionPrefix) {
		return "", "", false, nil
	}
	var pair [2]string
	raw, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ref, nativeVersionPrefix))
	if e != nil || json.Unmarshal(raw, &pair) != nil || pair[0] == "" || pair[1] == "" || strings.HasPrefix(pair[0], InternalPrefix) {
		return "", "", true, fmt.Errorf("invalid native version reference")
	}
	return pair[0], pair[1], true, nil
}
