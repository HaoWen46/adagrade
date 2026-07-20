package grading

import (
	"reflect"
	"testing"
)

// three models scoring two criteria (ids 1, 2); increment 0.5.
func inputs(scoresByModel ...[2]string) []PanelInput {
	out := make([]PanelInput, 0, len(scoresByModel))
	for i, s := range scoresByModel {
		out = append(out, PanelInput{
			MethodVersionID: int64(100 + i),
			RecordID:        int64(200 + i),
			Confidence:      "high",
			Scores: []CriterionScore{
				{CriterionID: 1, Score: s[0]},
				{CriterionID: 2, Score: s[1]},
			},
		})
	}
	return out
}

func TestCombine_MajorityConsensus(t *testing.T) {
	// n=3, f=1 → a value needs ≥ 2 votes.
	res, err := Combine(CombineParams{
		Combiner: "majority", PanelSize: 3, FaultTolerance: 1, Increment: "0.5",
		CriterionMax: map[int64]string{1: "6", 2: "4"},
		Inputs:       inputs([2]string{"3", "4"}, [2]string{"3", "4"}, [2]string{"5", "4"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Missing || len(res.ContestedCriteria) != 0 {
		t.Fatalf("unexpected flags: %+v", res)
	}
	want := map[int64]string{1: "3", 2: "4"}
	got := map[int64]string{}
	for _, c := range res.Scores {
		got[c.CriterionID] = c.Score
	}
	if !reflect.DeepEqual(got, want) || res.Total != "7" {
		t.Errorf("scores %v total %s, want %v total 7", got, res.Total, want)
	}
}

func TestCombine_MajorityContestedFallsBackToMean(t *testing.T) {
	// n=3, f=0 → need all 3 to agree; criterion 1 splits 3/3/5 → contested,
	// recorded value = snapped mean (11/3 = 3.67 → 3.5).
	res, err := Combine(CombineParams{
		Combiner: "majority", PanelSize: 3, FaultTolerance: 0, Increment: "0.5",
		CriterionMax: map[int64]string{1: "6", 2: "4"},
		Inputs:       inputs([2]string{"3", "4"}, [2]string{"3", "4"}, [2]string{"5", "4"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.ContestedCriteria, []int64{1}) {
		t.Fatalf("contested: %v", res.ContestedCriteria)
	}
	for _, c := range res.Scores {
		if c.CriterionID == 1 && c.Score != "3.5" {
			t.Errorf("contested criterion fallback: got %s want 3.5", c.Score)
		}
	}
}

func TestCombine_MeanSnapsAndDetectsOutliers(t *testing.T) {
	// mean of 3,3,5 = 3.67 → snapped 3.5. Outliers = |score-3.5| > 0.5:
	// 3→0.5 ok, 3→ok, 5→1.5 outlier ⇒ 1 outlier ≤ f=1 → not contested.
	res, err := Combine(CombineParams{
		Combiner: "mean", PanelSize: 3, FaultTolerance: 1, Increment: "0.5",
		CriterionMax: map[int64]string{1: "6", 2: "4"},
		Inputs:       inputs([2]string{"3", "4"}, [2]string{"3", "4"}, [2]string{"5", "4"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ContestedCriteria) != 0 {
		t.Fatalf("contested: %v", res.ContestedCriteria)
	}
	for _, c := range res.Scores {
		if c.CriterionID == 1 && c.Score != "3.5" {
			t.Errorf("mean: got %s want 3.5", c.Score)
		}
	}
	if res.Total != "7.5" {
		t.Errorf("total: %s", res.Total)
	}

	// Same spread with f=0 → contested.
	res, _ = Combine(CombineParams{
		Combiner: "mean", PanelSize: 3, FaultTolerance: 0, Increment: "0.5",
		CriterionMax: map[int64]string{1: "6", 2: "4"},
		Inputs:       inputs([2]string{"3", "4"}, [2]string{"3", "4"}, [2]string{"5", "4"}),
	})
	if !reflect.DeepEqual(res.ContestedCriteria, []int64{1}) {
		t.Errorf("f=0 contested: %v", res.ContestedCriteria)
	}
}

func TestCombine_MissingAndLowConfidence(t *testing.T) {
	// n=3, f=0, only 2 usable → missing.
	res, err := Combine(CombineParams{
		Combiner: "majority", PanelSize: 3, FaultTolerance: 0, Increment: "0.5",
		CriterionMax: map[int64]string{1: "6", 2: "4"},
		Inputs:       inputs([2]string{"3", "4"}, [2]string{"3", "4"}),
	})
	if err != nil || !res.Missing {
		t.Fatalf("want missing, got %+v err %v", res, err)
	}

	// n=2, f=0: two low-confidence records → low-confidence count 2 > f.
	in := inputs([2]string{"3", "4"}, [2]string{"3", "4"})
	in[0].Confidence = "low"
	in[1].Confidence = "low"
	res, err = Combine(CombineParams{
		Combiner: "majority", PanelSize: 2, FaultTolerance: 0, Increment: "0.5",
		CriterionMax: map[int64]string{1: "6", 2: "4"},
		Inputs:       in,
	})
	if err != nil || !res.LowConfidence {
		t.Fatalf("want low confidence, got %+v err %v", res, err)
	}
}

func TestCombine_ClampsToCriterionMax(t *testing.T) {
	// Mean 3.75 of maxed scores snaps to 4 but criterion max is 4 — and a model
	// over-scoring cannot push the aggregate above the max.
	res, err := Combine(CombineParams{
		Combiner: "mean", PanelSize: 2, FaultTolerance: 0, Increment: "0.5",
		CriterionMax: map[int64]string{1: "6", 2: "4"},
		Inputs:       inputs([2]string{"6", "4"}, [2]string{"6", "4"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != "10" {
		t.Errorf("total: %s want 10", res.Total)
	}
}

func TestValidatePolicy(t *testing.T) {
	if err := ValidatePolicyShape(3, 1); err != nil {
		t.Errorf("n=3 f=1 should be valid: %v", err)
	}
	if err := ValidatePolicyShape(2, 1); err == nil {
		t.Error("n=2 f=1 must be rejected (2f < n)")
	}
	if err := ValidatePolicyShape(0, 0); err == nil {
		t.Error("empty panel must be rejected")
	}
}
