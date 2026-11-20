package papers

import "strings"

// Write validation is opt-in metadata fixed in the approved question snapshot.
func validateSQLExecutionPolicy(mu *MultiError, q Question, prefix string) []string {
	raw, exists := q["sql_execution"]
	if !exists {
		return nil
	}
	policy, ok := raw.(map[string]any)
	if !ok {
		mu.add("sql_execution", prefix+": sql_execution must be an object")
		return nil
	}
	for field := range policy {
		if field != "mode" && field != "mutation_tables" {
			mu.add("sql_execution", prefix+": unknown sql_execution field "+field)
		}
	}
	mode := "query"
	if _, exists := policy["mode"]; exists {
		mode = stringOr(policy, "mode")
	}
	if mode != "query" && mode != "mutation" {
		mu.add("sql_execution", prefix+": SQL execution mode must be query or mutation")
		return nil
	}
	rawTables := []any{}
	if value, exists := policy["mutation_tables"]; exists {
		var valid bool
		rawTables, valid = value.([]any)
		if !valid {
			mu.add("sql_execution", prefix+": mutation_tables must be a list")
			return nil
		}
	}
	if len(rawTables) > 50 || (mode == "mutation" && len(rawTables) == 0) || (mode == "query" && len(rawTables) != 0) {
		mu.add("sql_execution", prefix+": only mutation mode requires 1 to 50 allowed tables")
	}
	tables := make([]string, 0, len(rawTables))
	seen := make(map[string]bool)
	for _, rawTable := range rawTables {
		table, valid := rawTable.(string)
		name := strings.ToLower(table)
		if !valid || !validSQLIdentifier(table) || seen[name] {
			mu.add("sql_execution", prefix+": mutation tables must be unique SQL identifiers")
			continue
		}
		seen[name] = true
		tables = append(tables, name)
	}
	if mode == "mutation" {
		if stringOr(q, "scoring_mode") != "sql" || stringOr(q, "sql_dialect") != "sqlite" {
			mu.add("sql_execution", prefix+": mutation mode requires SQL scoring and sqlite dialect")
		}
		datasets, valid := q["sql_test_datasets"].([]any)
		if !valid || len(datasets) < 2 {
			mu.add("sql_execution", prefix+": mutation mode requires at least two fixture datasets")
		}
		return tables
	}
	return nil
}
