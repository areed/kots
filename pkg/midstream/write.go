package midstream

import (
	"io/ioutil"
	"os"
	"path"
	"path/filepath"

	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/pkg/k8sdoc"
	"github.com/replicatedhq/kots/pkg/k8sutil"
	yaml "gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	kustomizetypes "sigs.k8s.io/kustomize/v3/pkg/types"
	k8syaml "sigs.k8s.io/yaml"
)

const (
	secretFilename  = "secret.yaml"
	patchesFilename = "pullsecrets.yaml"
)

type WriteOptions struct {
	MidstreamDir string
	BaseDir      string
}

func (m *Midstream) KustomizationFilename(options WriteOptions) string {
	return path.Join(options.MidstreamDir, "kustomization.yaml")
}

func (m *Midstream) WriteMidstream(options WriteOptions) error {
	var existingKustomization *kustomizetypes.Kustomization

	_, err := os.Stat(m.KustomizationFilename(options))
	if err == nil {
		k, err := k8sutil.ReadKustomizationFromFile(m.KustomizationFilename(options))
		if err != nil {
			return errors.Wrap(err, "load existing kustomization")
		}
		existingKustomization = k
	}

	if err := os.MkdirAll(options.MidstreamDir, 0744); err != nil {
		return errors.Wrap(err, "failed to mkdir")
	}

	secretFilename, err := m.writePullSecret(options)
	if err != nil {
		return errors.Wrap(err, "failed to write secret")
	}

	if secretFilename != "" {
		m.Kustomization.Resources = append(m.Kustomization.Resources, secretFilename)
	}

	patchFilename, err := m.writeObjectsWithPullSecret(options)
	if err != nil {
		return errors.Wrap(err, "failed to write patches")
	}
	if patchFilename != "" {
		m.Kustomization.PatchesStrategicMerge = append(m.Kustomization.PatchesStrategicMerge, kustomizetypes.PatchStrategicMerge(patchFilename))
	}

	m.mergeKustomization(existingKustomization)

	if err := m.writeKustomization(options); err != nil {
		return errors.Wrap(err, "failed to write kustomization")
	}

	return nil
}

func (m *Midstream) mergeKustomization(existing *kustomizetypes.Kustomization) {
	if existing == nil {
		return
	}

	filteredImages := removeExistingImages(m.Kustomization.Images, existing.Images)
	m.Kustomization.Images = append(m.Kustomization.Images, filteredImages...)

	newPatches := findNewPatches(m.Kustomization.PatchesStrategicMerge, existing.PatchesStrategicMerge)
	m.Kustomization.PatchesStrategicMerge = append(existing.PatchesStrategicMerge, newPatches...)

	newResources := findNewStrings(m.Kustomization.Resources, existing.Resources)
	m.Kustomization.Resources = append(existing.Resources, newResources...)
}

func (m *Midstream) writeKustomization(options WriteOptions) error {
	relativeBaseDir, err := filepath.Rel(options.MidstreamDir, options.BaseDir)
	if err != nil {
		return errors.Wrap(err, "failed to determine relative path for base from midstream")
	}

	fileRenderPath := m.KustomizationFilename(options)

	m.Kustomization.Bases = []string{
		relativeBaseDir,
	}

	if err := k8sutil.WriteKustomizationToFile(m.Kustomization, fileRenderPath); err != nil {
		return errors.Wrap(err, "failed to write kustomization to file")
	}

	return nil
}

func (m *Midstream) writePullSecret(options WriteOptions) (string, error) {
	if m.PullSecret == nil {
		return "", nil
	}

	absFilename := filepath.Join(options.MidstreamDir, secretFilename)

	b, err := k8syaml.Marshal(m.PullSecret)
	if err != nil {
		return "", errors.Wrap(err, "failed to marshal pull secret")
	}

	if err := ioutil.WriteFile(absFilename, b, 0644); err != nil {
		return "", errors.Wrap(err, "failed to write pull secret file")
	}

	return secretFilename, nil
}

func (m *Midstream) writeObjectsWithPullSecret(options WriteOptions) (string, error) {
	if len(m.DocForPatches) == 0 {
		return "", nil
	}

	filename := filepath.Join(options.MidstreamDir, patchesFilename)

	f, err := os.Create(filename)
	if err != nil {
		return "", errors.Wrap(err, "failed to create resources file")
	}
	defer f.Close()

	for _, o := range m.DocForPatches {
		withPullSecret := obejctWithPullSecret(o, m.PullSecret)

		b, err := yaml.Marshal(withPullSecret)
		if err != nil {
			return "", errors.Wrap(err, "failed to marshal object")
		}

		if _, err := f.Write([]byte("---\n")); err != nil {
			return "", errors.Wrap(err, "failed to write object")
		}
		if _, err := f.Write(b); err != nil {
			return "", errors.Wrap(err, "failed to write object")
		}
	}

	return patchesFilename, nil
}

func obejctWithPullSecret(obj *k8sdoc.Doc, secret *corev1.Secret) *k8sdoc.Doc {
	return &k8sdoc.Doc{
		APIVersion: obj.APIVersion,
		Kind:       obj.Kind,
		Metadata: k8sdoc.Metadata{
			Name: obj.Metadata.Name,
		},
		Spec: k8sdoc.Spec{
			Template: k8sdoc.Template{
				Spec: k8sdoc.PodSpec{
					ImagePullSecrets: []k8sdoc.ImagePullSecret{
						{"name": "kotsadm-replicated-registry"},
					},
				},
			},
		},
	}
}
