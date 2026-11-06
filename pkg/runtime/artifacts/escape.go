package artifacts

import (
	"errors"
	"fmt"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/workspacemedia"
)

type generatedMediaItem struct {
	workspaceRoot string
	// requestedPath is the prompt-directed target (untrusted marker or
	// user-prompt input, see chat.MediaDelta.RequestedPath); empty when
	// nothing named the blob explicitly.
	requestedPath string
	providerName  string
	genericName   string
	data          []byte
	mimeType      string
	agentName     string
	index, total  int
}

func (r *Service) writeGeneratedMedia(item generatedMediaItem, warn func(string)) (workspacemedia.Result, error) {
	if item.requestedPath == "" {
		return r.writeProviderNamedMedia(item)
	}

	class, cleaned := workspacemedia.ClassifyRequestedPath(item.requestedPath)
	if class == workspacemedia.PathWorkspaceRelative {
		res, err := r.Write(item.workspaceRoot, cleaned, item.data, item.mimeType)
		if err == nil || !errors.Is(err, workspacemedia.ErrPathEscape) {
			return res, err
		}
	}
	return r.redirectEscapedMedia(item, warn)
}

func (r *Service) writeProviderNamedMedia(item generatedMediaItem) (workspacemedia.Result, error) {
	requested := item.providerName
	if requested == "" {
		requested = item.genericName
	}
	res, err := r.Write(item.workspaceRoot, requested, item.data, item.mimeType)
	if err != nil && requested != item.genericName && errors.Is(err, workspacemedia.ErrPathEscape) {
		res, err = r.Write(item.workspaceRoot, item.genericName, item.data, item.mimeType)
	}
	return res, err
}

func (r *Service) redirectEscapedMedia(item generatedMediaItem, warn func(string)) (workspacemedia.Result, error) {
	base := workspacemedia.RequestedBasename(item.requestedPath)
	if base == "" {
		base = item.genericName
	}
	res, err := r.Write(item.workspaceRoot, base, item.data, item.mimeType)
	if err != nil && base != item.genericName && errors.Is(err, workspacemedia.ErrPathEscape) {
		res, err = r.Write(item.workspaceRoot, item.genericName, item.data, item.mimeType)
	}
	if err != nil {
		return workspacemedia.Result{}, err
	}
	if warn != nil {
		warning := fmt.Sprintf("Requested save location for generated media item %d/%d is outside the workspace or unusable; saved as %s in the workspace instead", item.index, item.total, res.RelPath)
		warn(chat.TruncateUTF8Bytes(warning, MaxWarningBytes))
	}
	return res, nil
}
