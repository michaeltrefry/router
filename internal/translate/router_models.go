package translate

// routerModelsTokens are the spellings of the router-models directive and its
// `models` alias, in both sigils. Codex reserves `/…` for its own built-ins
// and exposes the directive as a `$name` skill, so both are accepted for the
// same reason parseForceModelCommand accepts both.
var routerModelsTokens = [...]string{
	"/router-models",
	"$router-models",
	"/models",
	"$models",
}

// ExtractRouterModelsCommand reports whether the trailing user message is a
// bare router-models directive, stripping it so no upstream ever sees it.
//
// Bare only. Listing is a read the router can answer from the request it is
// already holding. Mutating the selection (`enable`/`disable`/`prefer`/
// `providers`) is a write to the /admin/v1 model-selection API, which only a
// self-hosted router mounts; anything with arguments falls through to the
// skill, which shells out to the installer so that boundary, and the
// managed-router refusal, live in one place.
func (env *RequestEnvelope) ExtractRouterModelsCommand() bool {
	return env.extractLeadingCommand(parseRouterModelsCommand)
}

// parseRouterModelsCommand recognizes the bare directive; parseBareDirective holds the
// rules, so a fix there reaches every directive at once.
func parseRouterModelsCommand(text string) (found bool, stripped string) {
	return parseBareDirective(text, routerModelsTokens[:])
}
