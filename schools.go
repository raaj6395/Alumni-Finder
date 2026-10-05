package main

// school: LinkedIn school id (for ?facetSchool=) plus the names alumni write on their profiles.
// X-ray ORs the first two variants into one query; each remaining variant gets its own query.
// To add a school, append a line here.
type school struct {
	Key      string   `json:"key"`
	Name     string   `json:"name"`
	ID       string   `json:"id"`
	Variants []string `json:"variants"`
}

var schools = []school{
	{"mnnit", "MNNIT Allahabad", "15105506", []string{"MNNIT", "Motilal Nehru National Institute of Technology", "MNNIT Allahabad", "NIT Allahabad"}},
	{"iitr", "IIT Roorkee", "157269", []string{"IIT Roorkee", "Indian Institute of Technology Roorkee", "University of Roorkee"}},
}

// schoolByKey: empty key means the first (default) school.
func schoolByKey(key string) (school, error) {
	if key == "" {
		return schools[0], nil
	}
	for _, s := range schools {
		if s.Key == key {
			return s, nil
		}
	}
	return school{}, &apiError{400, "bad_school", "unknown school " + key}
}
