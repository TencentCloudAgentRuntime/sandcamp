package sandcamp

import (
	"errors"
	"fmt"
	"path"
	"strings"

	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
)

const (
	sandcampRuntimeMountName = "sandcamp-runtime"
	sandcampRuntimeMountPath = "/mnt/sandcamp"
	sidecarMountNamePrefix   = "sandcamp-sidecar-"
	sidecarMountPathRoot     = "/mnt/sandcamp-sidecars"
)

var (
	ErrInvalidImage          = errors.New("invalid image")
	ErrDuplicateSidecarImage = errors.New("sidecar image name is duplicated")
	ErrMissingSidecarImage   = errors.New("sidecar process has no matching image")
)

// Image identifies the Sandcamp Runtime image mounted into every sandbox.
type Image struct {
	Reference         string
	ImageRegistryType string
}

// SidecarImage identifies one OCI image whose root filesystem is mounted as an
// Image Volume. Name must match the corresponding Process.Name.
type SidecarImage struct {
	Name              string
	Reference         string
	ImageRegistryType string
}

// ImageSet contains only images mounted as Image Volumes. The user's main
// business image remains in the AGS Tool CustomConfiguration.Image field.
type ImageSet struct {
	SandcampRuntime Image
	Sidecars        []SidecarImage
}

type resolvedImage struct {
	mountName         string
	mountPath         string
	reference         string
	imageRegistryType string
}

type resolvedImageSet struct {
	runtime  resolvedImage
	sidecars map[string]resolvedImage
	ordered  []resolvedImage
}

// RenderMounts generates the read-only Image StorageMounts required by
// Sandcamp. It performs no cloud API operation.
func RenderMounts(images ImageSet) ([]*ags.StorageMount, error) {
	resolved, err := resolveImageSet(images)
	if err != nil {
		return nil, err
	}
	mounts := make([]*ags.StorageMount, 0, 1+len(resolved.ordered))
	mounts = append(mounts, storageMount(resolved.runtime))
	for _, image := range resolved.ordered {
		mounts = append(mounts, storageMount(image))
	}
	return mounts, nil
}

func resolveImageSet(images ImageSet) (resolvedImageSet, error) {
	if err := validateImage(
		"sandcamp runtime",
		images.SandcampRuntime.Reference,
		images.SandcampRuntime.ImageRegistryType,
	); err != nil {
		return resolvedImageSet{}, err
	}
	result := resolvedImageSet{
		runtime: resolvedImage{
			mountName:         sandcampRuntimeMountName,
			mountPath:         sandcampRuntimeMountPath,
			reference:         images.SandcampRuntime.Reference,
			imageRegistryType: images.SandcampRuntime.ImageRegistryType,
		},
		sidecars: make(map[string]resolvedImage, len(images.Sidecars)),
		ordered:  make([]resolvedImage, 0, len(images.Sidecars)),
	}
	for index, image := range images.Sidecars {
		if !processNamePattern.MatchString(image.Name) {
			return resolvedImageSet{}, fmt.Errorf("%w: sidecars[%d].name is invalid", ErrInvalidImage, index)
		}
		if err := validateImage(
			"sidecar "+image.Name,
			image.Reference,
			image.ImageRegistryType,
		); err != nil {
			return resolvedImageSet{}, err
		}
		if _, exists := result.sidecars[image.Name]; exists {
			return resolvedImageSet{}, fmt.Errorf("%w: %s", ErrDuplicateSidecarImage, image.Name)
		}
		resolved := resolvedImage{
			mountName:         sidecarMountNamePrefix + image.Name,
			mountPath:         path.Join(sidecarMountPathRoot, image.Name),
			reference:         image.Reference,
			imageRegistryType: image.ImageRegistryType,
		}
		result.sidecars[image.Name] = resolved
		result.ordered = append(result.ordered, resolved)
	}
	return result, nil
}

func validateImage(label, reference, registryType string) error {
	if reference == "" ||
		reference != strings.TrimSpace(reference) ||
		strings.ContainsRune(reference, '\x00') {
		return fmt.Errorf("%w: %s reference is invalid", ErrInvalidImage, label)
	}
	if registryType != "personal" && registryType != "enterprise" {
		return fmt.Errorf("%w: %s registry type is invalid", ErrInvalidImage, label)
	}
	return nil
}

func storageMount(image resolvedImage) *ags.StorageMount {
	return &ags.StorageMount{
		Name:      stringPointer(image.mountName),
		MountPath: stringPointer(image.mountPath),
		ReadOnly:  boolPointer(true),
		StorageSource: &ags.StorageSource{
			Image: &ags.ImageStorageSource{
				Reference:         stringPointer(image.reference),
				ImageRegistryType: stringPointer(image.imageRegistryType),
			},
		},
	}
}

func boolPointer(value bool) *bool { return &value }
