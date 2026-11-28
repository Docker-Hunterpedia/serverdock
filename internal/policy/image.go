package policy

import (
	"errors"
	"strconv"
	"strings"
)

// ImageFacts is what the control plane learned from image_inspect.
type ImageFacts struct {
	// LabelKeys are label keys of the image (at least the reserved ones the
	// agent reports). Any key in the serverdock.* namespace refuses the image.
	LabelKeys    []string
	Volumes      []string // declared VOLUME paths
	ExposedPorts []string // "8080/tcp"
}

// ImageReport is the outcome of an accepted image.
type ImageReport struct {
	Warnings []string
	// SuggestedPort is the first exposed TCP port, 0 when there is none.
	SuggestedPort int
}

const maxWarnings = 8

// CheckImage refuses an image that carries any serverdock.* label (case-
// insensitive): such labels would merge into the container's and could make it
// look like one ServerDock owns, or fail the ownership rule so it can never be
// removed. A declared VOLUME is allowed and produces a warning (QM3a-3).
func CheckImage(f ImageFacts) (ImageReport, error) {
	for _, k := range f.LabelKeys {
		if IsReservedLabelKey(k) {
			return ImageReport{}, errors.New("the image carries serverdock.* labels (label spoofing rejected)")
		}
	}
	var rep ImageReport
	for _, v := range f.Volumes {
		if len(rep.Warnings) == maxWarnings {
			break
		}
		rep.Warnings = append(rep.Warnings,
			"data written to "+shown(v)+" is not kept between deploys until persistent volumes arrive")
	}
	for _, p := range f.ExposedPorts {
		n, proto, ok := strings.Cut(p, "/")
		if !ok || proto != "tcp" {
			continue
		}
		if port, err := strconv.Atoi(n); err == nil && ValidateContainerPort(port) == nil {
			rep.SuggestedPort = port
			break
		}
	}
	return rep, nil
}
