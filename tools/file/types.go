package file

import "encoding/json"

type FileStateEntry struct {
	Content       string  `json:"content"`
	TimeStamp     float64 `json:"timestamp"`
	IsPartialView bool    `json:"is_partial_view"`
	LineEndings   string  `json:"line_endings"`
}

func (f *FileStateEntry) ToMap() (map[string]any, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	dict := make(map[string]any)
	err = json.Unmarshal(b, &dict)
	if err != nil {
		return nil, err
	}
	return dict, nil
}

func GetFileEntryFromMap(m map[string]any) (FileStateEntry, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return FileStateEntry{}, err
	}
	var entry FileStateEntry
	err = json.Unmarshal(b, &entry)
	if err != nil {
		return FileStateEntry{}, err
	}
	return entry, nil
}
