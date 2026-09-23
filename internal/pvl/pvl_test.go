package pvl

import "testing"

func TestParse(t *testing.T) {
	label := `Object = IsisCube
  Object = Core
    StartByte = 65537 /* comment */
    Group = Dimensions
      Samples = 10
    End_Group
  End_Object
  Group = BandBin
    Center = (1.0, 2.0,
              3.0) <micrometers>
    Name   = "multi
              line"
    Radius = 2575000.0<meters>
    Flag
  End_Group
End_Object
End
garbage that must not be parsed = (`
	root, complete, err := Parse(label)
	if err != nil || !complete {
		t.Fatal(err, complete)
	}
	if v, _ := root.Child("IsisCube").Child("Core").Int("StartByte"); v != 65537 {
		t.Errorf("StartByte = %d", v)
	}
	if v, _ := root.Find("Dimensions").Int("samples"); v != 10 {
		t.Errorf("Samples = %d (keywords are case-insensitive)", v)
	}
	bb := root.Find("BandBin")
	if k := bb.Key("Center"); len(k.Values) != 3 || k.Values[2] != "3.0" {
		t.Errorf("Center = %v", k.Values)
	}
	if bb.Str("Name") != "multi line" {
		t.Errorf("Name = %q", bb.Str("Name"))
	}
	if bb.FloatOr("Radius", 0) != 2575000 {
		t.Errorf("Radius = %q (unit glued to the value)", bb.Str("Radius"))
	}
	if !bb.Has("Flag") || bb.Str("Flag") != "" {
		t.Errorf("keyword without a value")
	}
}

func TestParseTruncated(t *testing.T) {
	_, complete, err := Parse("Object = IsisCube\n  Group = Pixels\n    Type = Real\n")
	if err != nil || complete {
		t.Errorf("a label without End must parse but report incomplete: %v %v", err, complete)
	}
}

func TestParseNULPadding(t *testing.T) {
	root, _, err := Parse("Object = A\n  K = 1\n\x00\x00\x00binary")
	if err != nil || root.Child("A").Str("K") != "1" {
		t.Errorf("NUL padding should end the label: %v", err)
	}
}
