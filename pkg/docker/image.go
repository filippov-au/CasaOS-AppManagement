/*
credit: https://github.com/containrrr/watchtower
*/
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

func Image(ctx context.Context, imageName string) (*types.ImageInspect, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	imageInfo, _, err := cli.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		return nil, err
	}

	return &imageInfo, nil
}

func PullImage(ctx context.Context, imageName string, handleOut func(io.ReadCloser)) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()

	opts, err := GetPullOptions(imageName)
	if err != nil {
		return err
	}

	out, err := cli.ImagePull(ctx, imageName, opts)
	if err != nil {
		return err
	}
	defer out.Close()

	if handleOut == nil {
		return consumePullMessages(out, nil)
	}
	// Validate the daemon's JSON stream even when the progress callback ignores read errors.
	reader, writer := io.Pipe()
	result := make(chan error, 1)
	go func() {
		err := consumePullMessages(out, writer)
		writer.CloseWithError(err)
		result <- err
	}()
	handleOut(reader)
	reader.Close()
	return <-result

}

// Docker can return HTTP 200 followed by a JSON error during an image download.
func consumePullMessages(input io.Reader, progress io.Writer) error {
	decoder := json.NewDecoder(input)
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		var message struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			return err
		}
		if message.Error != "" {
			return fmt.Errorf("image pull failed: %s", message.Error)
		}
		if progress != nil {
			if _, err := progress.Write(append(raw, '\n')); err != nil {
				return err
			}
		}
	}
}

func HasNewImage(ctx context.Context, imageName string, currentImageID string) (bool, string, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return false, currentImageID, err
	}
	defer cli.Close()

	newImageInfo, _, err := cli.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		return false, currentImageID, err
	}

	newImageID := newImageInfo.ID
	if newImageID == currentImageID {
		return false, currentImageID, nil
	}

	return true, newImageID, nil
}
