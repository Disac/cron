package cron

func mapToSlice(entriesMap map[string]*Entry) (newEntries []*Entry) {
	newEntries = []*Entry{}
	for _, entry := range entriesMap {
		newEntries = append(newEntries, entry)
	}
	return
}
