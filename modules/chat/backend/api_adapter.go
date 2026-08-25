package backend

import "net/http"

var _ ServerInterface = (*Module)(nil)

func (m *Module) ChatCompletions(w http.ResponseWriter, r *http.Request) { m.completions(w, r) }
