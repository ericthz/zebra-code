package skills

// LoadBuiltins returns embedded skills compiled into the binary.
// Currently empty — all skills are loaded from disk at runtime
// (user-level ~/.zebracode/skills/ or project-level .zebracode/skills/).
func LoadBuiltins() []*Skill {
	return nil
}
