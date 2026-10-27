package engine

import (
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
)

// ErrProjectImageUnserved holds a writer attempt whose base no recorded
// image serves while image upkeep has nothing pending that could change
// that. It holds under admission_policy_refused with no check detail: the
// hold vocabulary has no typed check for it, and the attempt resumes on its
// own once an image serves the base.
var ErrProjectImageUnserved = errors.New("no admitted project image serves the run's base")

// ProjectImageSelection is what selecting the writer image reads. Every
// image here is decoded store state, so SelectProjectImage trusts none of
// it: each candidate is re-gated before it is chosen.
type ProjectImageSelection struct {
	// Proved is the image the runner backend currently proves; empty when
	// it proves none.
	Proved domain.ImageRef
	// Pointed is the image the admitted-image pointer names.
	Pointed domain.ProjectImage
	// Images are the repository's recorded images in record order.
	Images []domain.ProjectImage
	// BaseSHA is the commit the writer runs against.
	BaseSHA string
	// BaseInputs reads what BaseSHA holds for an input layout. It is called
	// only for a candidate built at another commit, so a start whose image
	// was built at the base needs no checkout.
	BaseInputs func(domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error)
	// Preparation is this binary's preparation digest.
	Preparation domain.Digest
}

// SelectProjectImage picks the image a writer attempt runs in (plan §5.7,
// Project-image upkeep), in this order:
//
//  1. The proved image, when it serves the base and keeps what the
//     pointed-to image binds, or when it is the pointed-to image and that
//     image is admissible under rule 2. Preferring it means the backend is
//     repointed only when the image it proves stops serving.
//  2. The pointed-to image, when it is admissible at the base and was
//     prepared with this binary's preparation sources (the rule
//     ImageUpkeep.current uses).
//  3. The first recorded image that serves the base and keeps what the
//     pointed-to image binds (rebuiltImageServes). Record order keeps the
//     choice stable across passes, and lets an older base be served by an
//     earlier write-once record.
//
// found is false when no image serves the base. An error is a failure to
// read the base, never a verdict on an image.
func SelectProjectImage(in ProjectImageSelection) (image domain.ProjectImage, found bool, err error) {
	inputs := map[domain.ProjectImageInputLayout]domain.ProjectImageBaseInputs{}
	baseInputs := func(layout domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error) {
		if observed, ok := inputs[layout]; ok {
			return observed, nil
		}
		observed, err := in.BaseInputs(layout)
		if err != nil {
			return domain.ProjectImageBaseInputs{}, fmt.Errorf("observe project-image inputs at base %s: %w", in.BaseSHA, err)
		}
		if observed.CommitSHA != in.BaseSHA {
			return domain.ProjectImageBaseInputs{}, fmt.Errorf(
				"project-image inputs were observed at %s, not the base %s",
				observed.CommitSHA, in.BaseSHA)
		}
		inputs[layout] = observed
		return observed, nil
	}
	pointedServes := func() (bool, error) {
		pointed := in.Pointed
		if pointed.Validate() != nil || !approvedPreparation(pointed) ||
			pointed.Environment == nil || pointed.Environment.PreparationDigest != in.Preparation {
			return false, nil
		}
		if pointed.CommitSHA == in.BaseSHA {
			return true, nil
		}
		observed, err := baseInputs(pointed.InputLayout())
		if err != nil {
			return false, err
		}
		return pointed.AdmissibleAt(observed, in.Preparation) == nil, nil
	}
	// rebuiltServes runs the checkout-free half of the gate first, so a
	// candidate that cannot qualify never costs a base read.
	rebuiltServes := func(candidate domain.ProjectImage) (bool, error) {
		if rebuiltImageKeepsAdmitted(candidate, in.Pointed) != nil {
			return false, nil
		}
		observed, err := baseInputs(candidate.InputLayout())
		if err != nil {
			return false, err
		}
		return rebuiltImageServes(candidate, in.Pointed, observed, in.Preparation) == nil, nil
	}

	if in.Proved != "" {
		if in.Proved == in.Pointed.ImageRef {
			serves, err := pointedServes()
			if err != nil || serves {
				return in.Pointed, serves, err
			}
		} else if index := slices.IndexFunc(in.Images, func(candidate domain.ProjectImage) bool {
			return candidate.ImageRef == in.Proved
		}); index >= 0 {
			serves, err := rebuiltServes(in.Images[index])
			if err != nil || serves {
				return in.Images[index], serves, err
			}
		}
	}
	if serves, err := pointedServes(); err != nil || serves {
		return in.Pointed, serves, err
	}
	for _, candidate := range in.Images {
		serves, err := rebuiltServes(candidate)
		if err != nil {
			return domain.ProjectImage{}, false, err
		}
		if serves {
			return candidate, true, nil
		}
	}
	return domain.ProjectImage{}, false, nil
}

// approvedPreparation re-runs the onboarding gate on a decoded image's
// preparation command, the argv the room runs as root: a record that names
// any other command is never selected.
func approvedPreparation(image domain.ProjectImage) bool {
	return slices.Equal(image.PreparationCommand, []string{projectimage.PreparationPath})
}
