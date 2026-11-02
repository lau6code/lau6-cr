package service

// Policy 是 tag 覆盖保护策略。
type Policy struct {
	PreventTagOverwrite    bool
	TagOverwriteExclusions []string
}

// Allows 判断某 tag 是否被豁免。
func (p Policy) Allows(tag string) bool {
	for _, t := range p.TagOverwriteExclusions {
		if t == tag {
			return true
		}
	}
	return false
}
