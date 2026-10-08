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
	"errors"
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

// ReviewRequest is what a mentor submits when reviewing a sign-off.
type ReviewRequest struct {
	// Rating is the mentor's overall assessment of the sign-off. It is
	// deliberately separate from the individual competency ratings: a mentor
	// may judge that the evidence supports competent performance overall
	// even when some individual competencies were rated developing.
	Rating Rating `json:"rating"`
	// Comment is the mentor's overall feedback. It is mandatory for the
	// same reason evidence is mandatory on individual assessments: a rating
	// without reasoning is a mark, not an assessment.
	Comment string `json:"comment"`
	// BaseRevision is the revision the client last saw. If non-zero, the
	// store uses it for optimistic concurrency control: the review is
	// applied only if the current revision matches, preventing a lost-update
	// when two mentors review the same sign-off concurrently.
	BaseRevision int64 `json:"base_revision,omitempty"`
}

// Reviewed is a review that has passed every rule, with the pieces the store
// needs filled in.
type Reviewed struct {
	ReviewRequest
	SignoffID  string
	Context    string
	ReviewerID string
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
	// Create stores a validated sign-off. grants carries the caller's full
	// context grants for the RLS policy, not just the active context.
	Create(ctx context.Context, callerID string, grants []string, v Validated) (Record, error)
	Get(ctx context.Context, context, callerID string, isAdmin bool, id string) (Record, bool, error)
	// Submit marks a draft as submitted for review. It is separate from Create
	// because the two acts have different permissions: a trainee may create a
	// draft, but only a mentor may submit it for review. activeContext is
	// supplied because the row itself cannot be read to discover it — the row
	// is invisible until the session variable carries the caller's grants.
	Submit(ctx context.Context, callerID string, id string, activeContext string) (Record, error)
	// Review records a mentor's review of a submitted sign-off. The review is
	// an assessment in its own right, with the same evidence requirements as
	// the original submission. activeContext is supplied for the same reason
	// Submit takes it: the store sets the session variable before touching
	// the row, and the row cannot be consulted for its own context first.
	Review(ctx context.Context, callerID string, id string, req ReviewRequest, activeContext string) (Record, error)
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

	// AdminRole is the group a programme administrator belongs to.
	//
	// Empty means nobody is one, which is the safe direction: a rule that widens
	// who may read somebody else's assessment is exactly the kind that must not
	// come into being by defaulting to something. It is a group rather than a
	// configured list of ids because group membership is already the authority for
	// what a person may reach, and a second list here could disagree with it.
	AdminRole string
}

// Get returns one sign-off to a caller entitled to see it.
//
// Three readers, and nobody else: the trainee it assesses, the mentor who wrote
// it, and a programme administrator in the same context. Anything else is
// reported as not found rather than as forbidden, so the answer to "is there a
// sign-off with this id" is the same whether the id is unknown or simply not this
// caller's — otherwise the endpoint becomes a way of learning which sign-offs
// exist.
//
// The rule lives in the store's statement rather than here. A caller of the store
// that forgot to apply it would return another trainee's honest assessment of
// themselves, and nothing else in this service would notice; a caller that
// forgot here would be caught by the store refusing.
func (s *Service) Get(ctx context.Context, callerID, activeContext string, isAdmin bool, id string) (Record, error) {
	if callerID == "" || activeContext == "" {
		// No write context is a refusal on create, but here it means the caller
		// cannot even be placed, and a read rule that matched everyone would be
		// the wrong answer to "who".
		return Record{}, refuse("no_read_context",
			"There is no context to read from, so nothing can be said to be yours.")
	}
	if !isUUID(id) {
		return Record{}, refuse("signoff_id_malformed",
			"That is not the identifier of a sign-off.")
	}
	rec, found, err := s.store.Get(ctx, activeContext, callerID, isAdmin, id)
	if err != nil {
		return Record{}, err
	}
	if !found {
		return Record{}, ErrNotFound
	}
	return rec, nil
}

// ErrNotFound means there is no sign-off this caller may see with that id.
var ErrNotFound = errors.New("signoff: not found")

// NotFound reports whether err is ErrNotFound.
func NotFound(err error) bool { return errors.Is(err, ErrNotFound) }

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
func (s *Service) Create(ctx context.Context, callerID, activeContext string, grants []string, req Request) (Record, error) {
	v, err := s.validate(ctx, activeContext, req)
	if err != nil {
		return Record{}, err
	}
	return s.store.Create(ctx, callerID, grants, v)
}

// Submit transitions a draft to submitted. The sign-off is now in the review
// queue and the review clock starts. Only the trainee who created the draft may
// submit it: a mentor submitting on a trainee's behalf would be a self-assessment,
// and a mentor who is not the trainee named on the record cannot know whether
// the trainee is ready to be judged.
func (s *Service) Submit(ctx context.Context, callerID, activeContext, id string) (Record, error) {
	if callerID == "" {
		return Record{}, refuse("no_caller",
			"There is no caller to hold accountable for this submission.")
	}
	if strings.TrimSpace(activeContext) == "" {
		return Record{}, refuse("no_submission_context",
			"This caller holds no context to submit in, so there is nowhere to record a submission.")
	}
	if !isUUID(id) {
		return Record{}, refuse("signoff_id_malformed",
			"That is not the identifier of a sign-off.")
	}
	return s.store.Submit(ctx, callerID, id, activeContext)
}

// Review records a mentor's assessment of a submitted sign-off. The review is
// itself an assessment, so it carries the same evidence requirements as the
// original submission: a rating without reasoning is a mark, not an assessment.
//
// The reviewer must be a mentor in the same context as the sign-off. A mentor
// from another context reviewing a sign-off they cannot see is the same
// boundary violation as a guide writing in a context they are not granted.
func (s *Service) Review(ctx context.Context, callerID, activeContext string, id string, req ReviewRequest) (Record, error) {
	if callerID == "" {
		return Record{}, refuse("no_caller",
			"There is no caller to hold accountable for this review.")
	}
	if !isUUID(id) {
		return Record{}, refuse("signoff_id_malformed",
			"That is not the identifier of a sign-off.")
	}
	if strings.TrimSpace(activeContext) == "" {
		return Record{}, refuse("no_review_context",
			"This caller holds no context to review in, so there is nowhere to record a review.")
	}
	if !knownRatings[req.Rating] {
		return Record{}, refuse("unknown_rating",
			"That is not one of the five ratings.")
	}
	if strings.TrimSpace(req.Comment) == "" {
		return Record{}, refuse("review_without_comment",
			"A review carries the mentor's reasoning, including one recorded as not observed — where it is why the competency could not be judged.")
	}

	// The review is validated as a whole: the sign-off must exist, be submitted,
	// and be in the caller's context. The store checks these in its transaction,
	// because a check outside the transaction could race with a concurrent
	// review or deletion.
	reviewed := Reviewed{
		ReviewRequest: req,
		SignoffID:     id,
		Context:       activeContext,
		ReviewerID:    callerID,
	}
	return s.store.Review(ctx, callerID, id, reviewed.ReviewRequest, activeContext)
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
