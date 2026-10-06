package openai

// IncludedOnlySubscriptions reports true: a local operator vouches that the
// subscriptions served through this adapter have paid extra usage disabled, so
// the provider itself rejects turns past the plan limit.
func (c *Client) IncludedOnlySubscriptions() bool { return true }
