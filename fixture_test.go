package fixture

import ("encoding/json"; "os"; "testing")
func TestSyntheticInput(t *testing.T) {
  data, err := os.ReadFile("fixture.json")
  if err != nil { t.Fatal(err) }
  var input struct { Synthetic bool; Events []struct{ID, Kind, Organization string} }
  if err := json.Unmarshal(data, &input); err != nil { t.Fatal(err) }
  if !input.Synthetic || len(input.Events) != 3 { t.Fatal("invalid input layout") }
  ids := map[string]bool{}
  for _, event := range input.Events {
    if ids[event.ID] || event.Kind == "" || event.Organization != "training" { t.Fatal(event) }
    ids[event.ID] = true
  }
}
