package api

import "fmt"

// SetUndeclaredConfigKeys wires the daemon config's keys that this daemon does
// not read (config.Config.UndeclaredKeys, GitHub #869). Before it is called the
// check reports SKIPPED: an unwired check must not read as clean.
func (h *DoctorHandlers) SetUndeclaredConfigKeys(keys []string) {
	h.undeclaredConfigKeys = append([]string(nil), keys...)
	h.undeclaredConfigKeysWired = true
}

// checkUndeclaredConfigKeys reports every config.yaml key this daemon does not
// read. The decode accepts such a key silently, so it reads as a setting and
// does nothing: chat.thinking_enabled did, for five months after it was
// removed (reasoning control design B7). Keys under a map or list, and under
// a section that decodes itself, are not judged (config.UnknownKeys).
func (h *DoctorHandlers) checkUndeclaredConfigKeys() DoctorCheck {
	const name = "config_undeclared_keys"
	if !h.undeclaredConfigKeysWired {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "config keys were not wired into this daemon"}
	}
	if len(h.undeclaredConfigKeys) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "every key in config.yaml is one this daemon reads"}
	}
	return DoctorCheck{
		Name:   name,
		Status: "WARNING",
		Message: fmt.Sprintf("%d config.yaml key(s) are not ones this daemon reads, so they have no effect "+
			"(removed, misspelt, or for another version); remove them or fix their spelling", len(h.undeclaredConfigKeys)),
		Items: h.undeclaredConfigKeys,
	}
}
