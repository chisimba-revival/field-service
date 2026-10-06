// Package signoff records a mentor's assessment of named competencies.
//
// Everything here is decided before the store is touched, for the same reason
// verification is: a submission that wrote the sign-off and then failed on a
// missing piece of evidence would leave a record whose assessments are partly
// the mentor's and partly absent, and a trainee reading it would have no way to
// tell which is which.
package signoff

import (
	"context"
	"strings"
)

// The bounds the contract sets.
//
// One is not one: a sign-off with a single assessment is a record of one thing
// observed, and the twenty is not a limit on ambition but on what a single
// piece of evidence can honestly speak to. Both are refusals rather than
// adjustments, because a client sending twenty-one is not making a sign-off, it
// is making two, and quietly keeping the first twenty would submit a record
// that differs from the one the mentor wrote.
const (
	MinAssessments = 1
	MaxAssessments = 20
)

// Rating is a mentor's judgement of one competency.
type Rating string

// The five ratings the contract names. not_observed is not the absence of a
// judgement: it is a judgement that the outing gave no opportunity to see the
// competency, which is why it carries evidence like the others.
const (
	NotObserved Rating = "not_observed"
	Developing  Rating = "developing"
	Competent   Rating = "competent"
	Proficient  Rating = "proficient"
	Expert      Rating = "expert"
)

var knownRatings = map[Rating]bool{
	NotObserved: true, Developing: true, Competent: true, Proficient: true, Expert: true,
}

// Assessment is one competency in a sign-off.
type Assessment struct {
	Code     string `json:"code"`
	Rating   Rating `json:"rating"`
	Evidence string `json:"evidence"`
}

// Request is what a mentor submits.
//
// TraineeID and MentorID are text, holding the logical Chisimba identifiers. They
// are never resolved to internal numbers here: the record a mentor reads names
// the people they know, and a sign-off that had to look up two rows to be legible
// would be a sign-off that reads wrong when a lookup fails.
type Request struct {
	OperationID    string       `json:"operation_id"`
	EntryID        string       `json:"signoff_id"`
	OutingID       string       `json:"outing_id"`
	TraineeID      string       `json:"trainee_id"`
	MentorID       string       `json:"mentor_id"`
	OverallComment string       `json:"overall_comment,omitempty"`
	Assessments    []Assessment `json:"competencies"`
	BaseRevision   int64        `json:"base_revision,omitempty"`
}

// Validated is a request that has passed every rule, with the pieces the store
// needs filled in.
//
// It is a distinct type so the store cannot be reached with an unvalidated
// request: the compiler refuses it, rather than the store trusting its caller.
type Validated struct {
	Request
	Context string
}

// A Refusal is a client mistake, named precisely.
type Refusal struct {
	Code    string
	Message string
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

func refuse(code, message string) error { return &Refusal{Code: code, Message: message} }

// Reviewer store interface.
type Store interface {
	Create(ctx context.Context, callerID string, v Validated) (Record, error)
}

// Service applies the rules.
type Service struct {
	store Store
	// Whether a competency code exists and is current. Supplied rather than
	// reached for, so the rule is decided here and the catalogue is one question
	// answered once instead of a query scattered through validation.
	Catalogue Catalogue
	// Whether an outing exists in a context.
	Outing func(ctx context.Context, outingID, context string) (bool, error)
}

// Catalogue answers whether a competency can be assessed.
type Catalogue interface {
	// Assessable reports whether code names a competency that may be rated now.
	// A retired competency is not assessable: it is still readable so history
	// survives, but offering it on a new sign-off would invite a mentor to assess
	// against a standard the curriculum has withdrawn.
	Assessable(ctx context.Context, codes []string) (map[string]bool, error)
}

// New builds a service.
func New(store Store, cat Catalogue, outing func(context.Context, string, string) (bool, error)) *Service {
	return &Service{store: store, Catalogue: cat, Outing: outing}
}

// Create validates and stores a draft.
//
// A draft, not a submission: submission starts the review clock, and the two are
// separate acts because a mentor's evidence is often still being written.
// Create validates and stores a draft.
//
// activeContext is passed in rather than derived. The authenticated Caller is the
// authority on which context a request writes in, and a service that looked it up
// again would be a second source for the single most consequential value in the
// row — with the two able to disagree, and the disagreement invisible.
func (s *Service) Create(ctx context.Context, callerID, activeContext string, req Request) (Record, error) {
	v, err := s.validate(ctx, activeContext, req)
	if err != nil {
		return Record{}, err
	}
	return s.store.Create(ctx, callerID, v)
}

// validate decides everything, in an order chosen so the most specific refusal
// is the one returned: a request naming no competency at all is told that,
// rather than being told about a missing trainee that was going to be the next
// refusal anyway.
func (s *Service) validate(ctx context.Context, context string, req Request) (Validated, error) {
	if strings.TrimSpace(req.OperationID) == "" {
		return Validated{}, refuse("signoff_without_operation_id",
			"A sign-off needs an operation_id, so a retry after a dropped connection is recognisably the same attempt.")
	}
	if strings.TrimSpace(req.EntryID) == "" {
		return Validated{}, refuse("signoff_without_id", "A sign-off needs its own id.")
	}
	if !isUUID(req.EntryID) {
		return Validated{}, refuse("signoff_id_malformed", "That sign-off id is not a UUID.")
	}
	if !isUUID(req.OutingID) {
		return Validated{}, refuse("signoff_without_outing_id",
			"A sign-off rests on an outing, and this one names none that could be found.")
	}
	if strings.TrimSpace(req.TraineeID) == "" {
		return Validated{}, refuse("signoff_without_trainee", "A sign-off assesses a named trainee.")
	}
	if strings.TrimSpace(req.MentorID) == "" {
		return Validated{}, refuse("signoff_without_mentor", "A sign-off names the mentor who made it.")
	}

	if len(req.Assessments) < MinAssessments || len(req.Assessments) > MaxAssessments {
		return Validated{}, refuse("signoff_without_assessments",
			"A sign-off carries between 1 and 20 assessments.")
	}

	// Duplicates are refused rather than merged. Two ratings of the same
	// competency in one sign-off means the mentor assessed it twice, and picking
	// either one would be choosing for them.
	seen := make(map[string]bool, len(req.Assessments))
	codes := make([]string, 0, len(req.Assessments))
	for i, a := range req.Assessments {
		code := strings.ToUpper(strings.TrimSpace(a.Code))
		if code == "" {
			return Validated{}, refuse("signoff_assessment_without_code",
				"Every assessment names a competency.")
		}
		if seen[code] {
			return Validated{}, refuse("signoff_assessment_duplicate",
				"A competency is assessed once per sign-off.")
		}
		seen[code] = true
		codes = append(codes, code)

		if !knownRatings[a.Rating] {
			// Refused, never defaulted. Defaulting an unknown rating to
			// not_observed would read as a mentor saying they saw nothing, which
			// is a judgement about a trainee rather than about the request.
			return Validated{}, refuse("unknown_rating",
				"That is not one of the five ratings.")
		}
		if strings.TrimSpace(a.Evidence) == "" {
			// Mandatory at every rating, and this is the rule the whole feature
			// rests on: evidence is what makes the record a mentor's assessment
			// rather than a grade. A sign-off that can be filed with no evidence
			// is a self-assessment with a superior's name on it.
			return Validated{}, refuse("signoff_assessment_without_evidence",
				"Every assessment carries evidence, including one recorded as not observed — where it is why the competency could not be judged.")
		}
		_ = i
	}

	// The codes are checked against the catalogue only after the shape is known
	// to be right, so a client with a typo is told about the typo and not about
	// the four other things it also did.
	if s.Catalogue != nil {
		ok, err := s.Catalogue.Assessable(ctx, codes)
		if err != nil {
			return Validated{}, err
		}
		for _, code := range codes {
			if !ok[code] {
				// One code for both "no such competency" and "no longer
				// offered". Distinguishing them would tell a caller whether a code
				// was ever real, which is a small thing to give away and a thing
				// that would be needed to enumerate the curriculum.
				return Validated{}, refuse("unknown_competency",
					"One of those competency codes is not one this installation offers.")
			}
		}
	}

	// The write context is the caller's active context, never a value from the
	// request. There is no context field on Request at all, which is the same
	// decision rule 2 made about sightings and is enforced by the type here rather
	// than by remembering to ignore a field.
	if strings.TrimSpace(context) == "" {
		return Validated{}, refuse("no_write_context",
			"This caller holds no context to write in, so there is nowhere to record a sign-off.")
	}

	if s.Outing != nil {
		found, err := s.Outing(ctx, req.OutingID, context)
		if err != nil {
			return Validated{}, err
		}
		if !found {
			// Refused, and phrased so it is identical whether the outing does not
			// exist or belongs to another context. A message that distinguished
			// them would be an oracle for walking the id space to discover other
			// reserves' outings.
			return Validated{}, refuse("unknown_outing",
				"That outing is not one this caller can assess against.")
		}
	}

	v := Validated{Request: req, Context: context}
	// Codes are normalised in place so the store writes what was validated rather
	// than re-deriving it, and a code differing from its validated form by case is
	// impossible rather than merely unlikely.
	for i := range v.Assessments {
		v.Assessments[i].Code = strings.ToUpper(strings.TrimSpace(v.Assessments[i].Code))
	}
	return v, nil
}
