package main

// Explicit API credentials authorize caller payment; ambient cookies never do.
// Previews stay owner-paid and cannot charge a different account's API key.
func (s *Server) hostedFunctionPayer(fn functionManifest, draft bool, ownerID, apiKey string) *User {
	if fn.Billing == "caller" && !draft {
		return s.auth.userFromAPIKey(apiKey)
	}
	if fn.Billing != "" && fn.Billing != "owner" && fn.Billing != "caller" {
		return nil
	}
	return s.auth.userByID(ownerID)
}
