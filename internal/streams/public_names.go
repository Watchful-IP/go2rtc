package streams

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Process-local HMACs cannot be used to guess credentials offline. Aliases stay
// stable until deletion, including when an explicit-name collision is removed.
var publicNameKey = func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}()

var publicNames = map[string]string{}
var privateNames = map[string]string{}

func protectedName(name string) bool {
	scheme, _, _ := strings.Cut(name, ":")
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "exacq", "exacqsdk", "exacqsdks":
		return true
	}
	return false
}

// Callers hold streamsMu. Raw keys retain configuration and legacy identity.
func publicNameLocked(name string) string {
	if !protectedName(name) {
		return name
	}
	if alias := publicNames[name]; alias != "" {
		return alias
	}
	hash := hmac.New(sha256.New, publicNameKey)
	_, _ = hash.Write([]byte(name))
	alias := "exacq-stream-" + hex.EncodeToString(hash.Sum(nil))
	for streams[alias] != nil || privateNames[alias] != "" {
		alias += "-"
	}
	publicNames[name], privateNames[alias] = alias, name
	return alias
}

func resolveNameLocked(name string) string {
	if raw := privateNames[name]; raw != "" && streams[raw] != nil {
		return raw
	}
	return name
}

func forgetPublicNameLocked(raw string) {
	delete(privateNames, publicNames[raw])
	delete(publicNames, raw)
}

func resolveName(name string) string {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	return resolveNameLocked(name)
}
