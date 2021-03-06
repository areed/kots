package base

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"

	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/pkg/k8sutil"
	kustomizetypes "sigs.k8s.io/kustomize/v3/pkg/types"
)

type WriteOptions struct {
	BaseDir          string
	Overwrite        bool
	ExcludeKotsKinds bool
}

func (b *Base) WriteBase(options WriteOptions) error {
	renderDir := options.BaseDir

	_, err := os.Stat(renderDir)
	if err == nil {
		if options.Overwrite {
			if err := os.RemoveAll(renderDir); err != nil {
				return errors.Wrap(err, "failed to remove previous content in base")
			}
		} else {
			return fmt.Errorf("directory %s already exists", renderDir)
		}
	}

	kustomizeResources := []string{}
	for _, file := range b.Files {
		writeToBase := file.ShouldBeIncludedInBaseFilesystem(options.ExcludeKotsKinds)
		writeToKustomization := file.ShouldBeIncludedInBaseKustomization(options.ExcludeKotsKinds)

		if !writeToBase && !writeToKustomization {
			continue
		}

		if writeToKustomization {
			kustomizeResources = append(kustomizeResources, path.Join(".", file.Path))
		}

		if writeToBase {
			fileRenderPath := path.Join(renderDir, file.Path)
			d, _ := path.Split(fileRenderPath)
			if _, err := os.Stat(d); os.IsNotExist(err) {
				if err := os.MkdirAll(d, 0744); err != nil {
					return errors.Wrap(err, "failed to mkdir")
				}
			}

			if err := ioutil.WriteFile(fileRenderPath, file.Content, 0644); err != nil {
				return errors.Wrap(err, "failed to write base file")
			}
		}
	}

	kustomization := kustomizetypes.Kustomization{
		TypeMeta: kustomizetypes.TypeMeta{
			APIVersion: "kustomize.config.k8s.io/v1beta1",
			Kind:       "Kustomization",
		},
		Resources: kustomizeResources,
	}

	if err := k8sutil.WriteKustomizationToFile(&kustomization, path.Join(renderDir, "kustomization.yaml")); err != nil {
		return errors.Wrap(err, "failed to write kustomization to file")
	}

	return nil
}

func (b *Base) GetOverlaysDir(options WriteOptions) string {
	renderDir := options.BaseDir

	return path.Join(renderDir, "..", "overlays")
}
