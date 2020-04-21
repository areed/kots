package upload

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/manifoldco/promptui"
	"github.com/pkg/errors"
	kotsscheme "github.com/replicatedhq/kots/kotskinds/client/kotsclientset/scheme"
	"github.com/replicatedhq/kots/pkg/auth"
	"github.com/replicatedhq/kots/pkg/docker/registry"
	"github.com/replicatedhq/kots/pkg/logger"
	"github.com/replicatedhq/kots/pkg/util"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes/scheme"
)

type UploadOptions struct {
	Namespace             string
	UpstreamURI           string
	KubernetesConfigFlags *genericclioptions.ConfigFlags
	ExistingAppSlug       string
	NewAppName            string
	RegistryOptions       registry.RegistryOptions
	Endpoint              string
	Silent                bool
	updateCursor          string
	license               *string
	versionLabel          string
}

func init() {
	kotsscheme.AddToScheme(scheme.Scheme)
}

// Upload will upload the application version at path
// using the options in uploadOptions
func Upload(path string, uploadOptions UploadOptions) error {
	license, err := findLicense(path)
	if err != nil {
		return errors.Wrap(err, "failed to find license")
	}
	uploadOptions.license = license

	updateCursor, err := findUpdateCursor(path)
	if err != nil {
		return errors.Wrap(err, "failed to find update cursor")
	}
	if updateCursor == "" {
		return errors.New("no update cursor found. this is not yet supported")
	}
	uploadOptions.updateCursor = updateCursor

	archiveFilename, err := createUploadableArchive(path)
	if err != nil {
		return errors.Wrap(err, "failed to create uploadable archive")
	}

	defer os.Remove(archiveFilename)

	// Make sure we have a name or slug
	if uploadOptions.ExistingAppSlug == "" && uploadOptions.NewAppName == "" {
		split := strings.Split(path, string(os.PathSeparator))
		lastPathPart := ""
		idx := 1
		for lastPathPart == "" {
			lastPathPart = split[len(split)-idx]
			if lastPathPart == "" && len(split) > idx {
				idx++
				continue
			}

			break
		}

		appName, err := relentlesslyPromptForAppName(lastPathPart)
		if err != nil {
			return errors.Wrap(err, "failed to prompt for app name")
		}

		uploadOptions.NewAppName = appName
	}

	// Make sure we have an upstream URI
	if uploadOptions.ExistingAppSlug == "" && uploadOptions.UpstreamURI == "" {
		upstreamURI, err := promptForUpstreamURI()
		if err != nil {
			return errors.Wrap(err, "failed to prompt for upstream uri")
		}

		uploadOptions.UpstreamURI = upstreamURI
	}

	// Find the kotadm-api pod
	log := logger.NewLogger()
	if uploadOptions.Silent {
		log.Silence()
	}

	log.ActionWithSpinner("Uploading local application to Admin Console")

	// PUT /api/v1/upload is implemented in the GO API
	// POST /api/v1/kots/ for new installs so the request gets forwarded to the Node API
	endpoint := fmt.Sprintf("%s/api/v1/upload", uploadOptions.Endpoint)
	if uploadOptions.ExistingAppSlug == "" {
		endpoint = fmt.Sprintf("%s/api/v1/kots/", uploadOptions.Endpoint)
	}

	// upload using http to the pod directly
	req, err := createUploadRequest(archiveFilename, uploadOptions, endpoint)
	if err != nil {
		log.FinishSpinnerWithError()
		return errors.Wrap(err, "failed to create upload request")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.FinishSpinnerWithError()
		return errors.Wrap(err, "failed to execute request")
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.FinishSpinnerWithError()
		return errors.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.FinishSpinnerWithError()
		return errors.Wrap(err, "failed to read response body")
	}
	type UploadResponse struct {
		Slug string `json:"slug"`
	}
	var uploadResponse UploadResponse
	if err := json.Unmarshal(b, &uploadResponse); err != nil {
		log.FinishSpinnerWithError()
		return errors.Wrap(err, "failed to unmarshal response")
	}

	log.FinishSpinner()

	return nil
}

func createUploadRequest(path string, uploadOptions UploadOptions, uri string) (*http.Request, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open file")
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	archivePart, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create form file")
	}
	_, err = io.Copy(archivePart, file)
	if err != nil {
		return nil, errors.Wrap(err, "failed to copy file to upload")
	}

	method := ""
	if uploadOptions.ExistingAppSlug != "" {
		method = "PUT"
		metadata := map[string]string{
			"slug":         uploadOptions.ExistingAppSlug,
			"versionLabel": uploadOptions.versionLabel,
			"updateCursor": uploadOptions.updateCursor,
			// Intentionally not including registry info here.  Updating settings should be its own thing.
		}
		b, err := json.Marshal(metadata)
		if err != nil {
			return nil, errors.Wrap(err, "failed to marshal json")
		}
		metadataPart, err := writer.CreateFormField("metadata")
		if err != nil {
			return nil, errors.Wrap(err, "failed to add metadata")
		}
		if _, err := io.Copy(metadataPart, bytes.NewReader(b)); err != nil {
			return nil, errors.Wrap(err, "failed to copy metadata")
		}
	} else {
		method = "POST"

		metadata := map[string]string{
			"name":              uploadOptions.NewAppName,
			"versionLabel":      uploadOptions.versionLabel,
			"upstreamURI":       uploadOptions.UpstreamURI,
			"updateCursor":      uploadOptions.updateCursor,
			"registryEndpoint":  uploadOptions.RegistryOptions.Endpoint,
			"registryUsername":  uploadOptions.RegistryOptions.Username,
			"registryPassword":  uploadOptions.RegistryOptions.Password,
			"registryNamespace": uploadOptions.RegistryOptions.Namespace,
		}

		if uploadOptions.license != nil {
			metadata["license"] = *uploadOptions.license
		}

		b, err := json.Marshal(metadata)
		if err != nil {
			return nil, errors.Wrap(err, "failed to marshal json")
		}
		metadataPart, err := writer.CreateFormField("metadata")
		if err != nil {
			return nil, errors.Wrap(err, "failed to add metadata")
		}
		if _, err := io.Copy(metadataPart, bytes.NewReader(b)); err != nil {
			return nil, errors.Wrap(err, "failed to copy metadata")
		}
	}

	err = writer.Close()
	if err != nil {
		return nil, errors.Wrap(err, "failed to close writer")
	}

	authSlug, err := auth.GetOrCreateAuthSlug(uploadOptions.KubernetesConfigFlags, uploadOptions.Namespace)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get auth slug")
	}

	req, err := http.NewRequest(method, uri, body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create new request")
	}

	req.Header.Set("Authorization", authSlug)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req, nil
}

func relentlesslyPromptForAppName(defaultAppName string) (string, error) {
	templates := &promptui.PromptTemplates{
		Prompt:  "{{ . | bold }} ",
		Valid:   "{{ . | green }} ",
		Invalid: "{{ . | red }} ",
		Success: "{{ . | bold }} ",
	}

	prompt := promptui.Prompt{
		Label:     "Application name:",
		Templates: templates,
		Default:   defaultAppName,
		Validate: func(input string) error {
			if len(input) < 3 {
				return errors.New("invalid app name")
			}
			return nil
		},
	}

	for {
		result, err := prompt.Run()
		if err != nil {
			if err == promptui.ErrInterrupt {
				os.Exit(-1)
			}
			continue
		}

		return result, nil
	}
}

func promptForUpstreamURI() (string, error) {
	templates := &promptui.PromptTemplates{
		Prompt:  "{{ . | bold }} ",
		Valid:   "{{ . | green }} ",
		Invalid: "{{ . | red }} ",
		Success: "{{ . | bold }} ",
	}

	supportedSchemes := map[string]interface{}{
		"helm":       nil,
		"replicated": nil,
	}

	prompt := promptui.Prompt{
		Label:     "Upstream URI:",
		Templates: templates,
		Validate: func(input string) error {
			if !util.IsURL(input) {
				return errors.New("Please enter a URL")
			}

			u, err := url.ParseRequestURI(input)
			if err != nil {
				return errors.New("Invalid URL")
			}

			_, ok := supportedSchemes[u.Scheme]
			if !ok {
				return errors.New("Unsupported upstream type")
			}

			return nil
		},
	}

	for {
		result, err := prompt.Run()
		if err != nil {
			if err == promptui.ErrInterrupt {
				os.Exit(-1)
			}
			continue
		}

		return result, nil
	}
}
