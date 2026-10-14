package postgres

// nullableGoalOrigin stores an unset goal origin as NULL, which the skill
// distiller reads as unknown and refuses (GitHub #347, migration 230).
func nullableGoalOrigin(origin string) any {
	if origin == "" {
		return nil
	}
	return origin
}
