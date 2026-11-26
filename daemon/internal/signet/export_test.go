package signet

// HTTPRoutePatterns returns the pattern of every route NewHTTPHandler
// registers, in registration order, so the external test package can probe
// each one. A zero httpHandler only binds method values here; no handler runs.
func HTTPRoutePatterns() []string {
	routes := httpHandler{}.routes()
	patterns := make([]string, 0, len(routes))
	for _, route := range routes {
		patterns = append(patterns, route.pattern)
	}
	return patterns
}

// EmbeddedOpenAPISpec returns the embedded api/openapi.yaml bytes so the
// external test package can verify them byte for byte against the repo spec.
func EmbeddedOpenAPISpec() []byte {
	return embeddedOpenAPISpec
}
