package service

// restartPendingReasons is the reader the API's setup/status uses (GitHub
// #794, operations-API design §6.12 R1/R2): the UI server's restart-pending
// codes ("reload", "model_provider") and config:<path> for each restart-only
// field whose on-disk value differs from what is running. Names only.
func (c *Container) restartPendingReasons() []string {
	var out []string
	if u := c.uiServer.Load(); u != nil {
		out = append(out, u.RestartPendingCodes()...)
	}
	if c.ConfigReloader != nil {
		for _, p := range c.ConfigReloader.PendingRestartOnlyPaths() {
			out = append(out, "config:"+p)
		}
	}
	return out
}
