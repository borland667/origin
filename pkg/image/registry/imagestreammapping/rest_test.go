package imagestreammapping

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	kapi "github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/errors"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/tools"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/tools/etcdtest"
	"github.com/coreos/go-etcd/etcd"

	"github.com/openshift/origin/pkg/api/latest"
	authorizationapi "github.com/openshift/origin/pkg/authorization/api"
	"github.com/openshift/origin/pkg/authorization/registry/subjectaccessreview"
	"github.com/openshift/origin/pkg/image/api"
	"github.com/openshift/origin/pkg/image/registry/image"
	imageetcd "github.com/openshift/origin/pkg/image/registry/image/etcd"
	"github.com/openshift/origin/pkg/image/registry/imagestream"
	imagestreametcd "github.com/openshift/origin/pkg/image/registry/imagestream/etcd"
)

var testDefaultRegistry = imagestream.DefaultRegistryFunc(func() (string, bool) { return "defaultregistry:5000", true })

type fakeSubjectAccessReviewRegistry struct {
}

var _ subjectaccessreview.Registry = &fakeSubjectAccessReviewRegistry{}

func (f *fakeSubjectAccessReviewRegistry) CreateSubjectAccessReview(ctx kapi.Context, subjectAccessReview *authorizationapi.SubjectAccessReview) (*authorizationapi.SubjectAccessReviewResponse, error) {
	return nil, nil
}

func setup(t *testing.T) (*tools.FakeEtcdClient, tools.EtcdHelper, *REST) {
	fakeEtcdClient := tools.NewFakeEtcdClient(t)
	fakeEtcdClient.TestIndex = true
	helper := tools.NewEtcdHelper(fakeEtcdClient, latest.Codec, etcdtest.PathPrefix())
	imageStorage := imageetcd.NewREST(helper)
	imageRegistry := image.NewRegistry(imageStorage)
	imageStreamStorage, imageStreamStatus := imagestreametcd.NewREST(helper, testDefaultRegistry, &fakeSubjectAccessReviewRegistry{})
	imageStreamRegistry := imagestream.NewRegistry(imageStreamStorage, imageStreamStatus)
	storage := NewREST(imageRegistry, imageStreamRegistry)
	return fakeEtcdClient, helper, storage
}

func validImageStream() *api.ImageStream {
	return &api.ImageStream{
		ObjectMeta: kapi.ObjectMeta{
			Name: "test",
		},
	}
}

func validNewMappingWithName() *api.ImageStreamMapping {
	return &api.ImageStreamMapping{
		ObjectMeta: kapi.ObjectMeta{
			Namespace: "default",
			Name:      "somerepo",
		},
		Image: api.Image{
			ObjectMeta: kapi.ObjectMeta{
				Name: "imageID1",
			},
			DockerImageReference: "localhost:5000/default/somerepo:imageID1",
			DockerImageMetadata: api.DockerImage{
				Config: api.DockerConfig{
					Cmd:          []string{"ls", "/"},
					Env:          []string{"a=1"},
					ExposedPorts: map[string]struct{}{"1234/tcp": {}},
					Memory:       1234,
					CPUShares:    99,
					WorkingDir:   "/workingDir",
				},
			},
		},
		Tag: "latest",
	}
}

func TestCreateConflictingNamespace(t *testing.T) {
	_, _, storage := setup(t)

	mapping := validNewMappingWithName()
	mapping.Namespace = "some-value"

	ch, err := storage.Create(kapi.WithNamespace(kapi.NewContext(), "legal-name"), mapping)
	if ch != nil {
		t.Error("Expected a nil obj, but we got a value")
	}
	expectedError := "the namespace of the provided object does not match the namespace sent on the request"
	if err == nil {
		t.Fatalf("Expected '" + expectedError + "', but we didn't get one")
	}
	if !strings.Contains(err.Error(), expectedError) {
		t.Errorf("Expected '"+expectedError+"' error, got '%v'", err.Error())
	}
}

func TestCreateImageStreamNotFoundWithName(t *testing.T) {
	fakeEtcdClient, _, storage := setup(t)
	fakeEtcdClient.ExpectNotFoundGet("/imagerepositories/default/somerepo")

	obj, err := storage.Create(kapi.NewDefaultContext(), validNewMappingWithName())
	if obj != nil {
		t.Errorf("Unexpected non-nil obj %#v", obj)
	}
	if err == nil {
		t.Fatal("Unexpected nil err")
	}
	e, ok := err.(*errors.StatusError)
	if !ok {
		t.Fatalf("expected StatusError, got %#v", err)
	}
	if e, a := http.StatusNotFound, e.ErrStatus.Code; e != a {
		t.Errorf("error status code: expected %d, got %d", e, a)
	}
	if e, a := "imageStream", e.ErrStatus.Details.Kind; e != a {
		t.Errorf("error status details kind: expected %s, got %s", e, a)
	}
	if e, a := "somerepo", e.ErrStatus.Details.ID; e != a {
		t.Errorf("error status details name: expected %s, got %s", e, a)
	}
}

func TestCreateSuccessWithName(t *testing.T) {
	fakeEtcdClient, helper, storage := setup(t)

	initialRepo := &api.ImageStream{
		ObjectMeta: kapi.ObjectMeta{Namespace: "default", Name: "somerepo"},
	}

	fakeEtcdClient.Data["/imagerepositories/default/somerepo"] = tools.EtcdResponseWithError{
		R: &etcd.Response{
			Node: &etcd.Node{
				Value:         runtime.EncodeOrDie(latest.Codec, initialRepo),
				ModifiedIndex: 1,
			},
		},
	}

	mapping := validNewMappingWithName()
	_, err := storage.Create(kapi.NewDefaultContext(), mapping)
	if err != nil {
		t.Fatalf("Unexpected error creating mapping: %#v", err)
	}

	image := &api.Image{}
	if err := helper.ExtractObj("/images/imageID1", image, false); err != nil {
		t.Errorf("Unexpected error retrieving image: %#v", err)
	}
	if e, a := mapping.Image.DockerImageReference, image.DockerImageReference; e != a {
		t.Errorf("Expected %s, got %s", e, a)
	}
	if !reflect.DeepEqual(mapping.Image.DockerImageMetadata, image.DockerImageMetadata) {
		t.Errorf("Expected %#v, got %#v", mapping.Image, image)
	}

	repo := &api.ImageStream{}
	if err := helper.ExtractObj("/imagerepositories/default/somerepo", repo, false); err != nil {
		t.Errorf("Unexpected non-nil err: %#v", err)
	}
	if e, a := "imageID1", repo.Status.Tags["latest"].Items[0].Image; e != a {
		t.Errorf("Expected %s, got %s", e, a)
	}
}

func TestAddExistingImageWithNewTag(t *testing.T) {
	imageID := "8d812da98d6dd61620343f1a5bf6585b34ad6ed16e5c5f7c7216a525d6aeb772"
	existingRepo := &api.ImageStream{
		ObjectMeta: kapi.ObjectMeta{
			Name:      "somerepo",
			Namespace: "default",
		},
		Spec: api.ImageStreamSpec{
			DockerImageRepository: "localhost:5000/someproject/somerepo",
			/*
				Tags: map[string]api.TagReference{
					"existingTag": {
						From: &kapi.ObjectReference{
							Kind: "ImageStreamTag",

						Tag: "existingTag", Reference: imageID},
				},
			*/
		},
		Status: api.ImageStreamStatus{
			Tags: map[string]api.TagEventList{
				"existingTag": {Items: []api.TagEvent{{DockerImageReference: "localhost:5000/someproject/somerepo:" + imageID}}},
			},
		},
	}

	existingImage := &api.Image{
		ObjectMeta: kapi.ObjectMeta{
			Name:      imageID,
			Namespace: "default",
		},
		DockerImageReference: "localhost:5000/someproject/somerepo:" + imageID,
		DockerImageMetadata: api.DockerImage{
			Config: api.DockerConfig{
				Cmd:          []string{"ls", "/"},
				Env:          []string{"a=1"},
				ExposedPorts: map[string]struct{}{"1234/tcp": {}},
				Memory:       1234,
				CPUShares:    99,
				WorkingDir:   "/workingDir",
			},
		},
	}

	fakeEtcdClient, _, storage := setup(t)
	fakeEtcdClient.Data["/imagerepositories/default"] = tools.EtcdResponseWithError{
		R: &etcd.Response{
			Node: &etcd.Node{
				Nodes: []*etcd.Node{
					{
						Value:         runtime.EncodeOrDie(latest.Codec, existingRepo),
						ModifiedIndex: 1,
					},
				},
			},
		},
	}
	fakeEtcdClient.Data["/imagerepositories/default/somerepo"] = tools.EtcdResponseWithError{
		R: &etcd.Response{
			Node: &etcd.Node{
				Value:         runtime.EncodeOrDie(latest.Codec, existingRepo),
				ModifiedIndex: 1,
			},
		},
	}
	fakeEtcdClient.Data["/images/default/"+imageID] = tools.EtcdResponseWithError{
		R: &etcd.Response{
			Node: &etcd.Node{
				Value:         runtime.EncodeOrDie(latest.Codec, existingImage),
				ModifiedIndex: 1,
			},
		},
	}

	mapping := api.ImageStreamMapping{
		Image: *existingImage,
		Tag:   "latest",
	}
	_, err := storage.Create(kapi.NewDefaultContext(), &mapping)
	if !errors.IsInvalid(err) {
		t.Fatalf("Unexpected non-error creating mapping: %#v", err)
	}
}

func TestAddExistingImageAndTag(t *testing.T) {
	existingRepo := &api.ImageStream{
		ObjectMeta: kapi.ObjectMeta{
			Name:      "somerepo",
			Namespace: "default",
		},
		Spec: api.ImageStreamSpec{
			DockerImageRepository: "localhost:5000/someproject/somerepo",
			/*
				Tags: map[string]api.TagReference{
					"existingTag": {Tag: "existingTag", Reference: "existingImage"},
				},
			*/
		},
		Status: api.ImageStreamStatus{
			Tags: map[string]api.TagEventList{
				"existingTag": {Items: []api.TagEvent{{DockerImageReference: "existingImage"}}},
			},
		},
	}

	existingImage := &api.Image{
		ObjectMeta: kapi.ObjectMeta{
			Name:      "existingImage",
			Namespace: "default",
		},
		DockerImageReference: "localhost:5000/someproject/somerepo:imageID1",
		DockerImageMetadata: api.DockerImage{
			Config: api.DockerConfig{
				Cmd:          []string{"ls", "/"},
				Env:          []string{"a=1"},
				ExposedPorts: map[string]struct{}{"1234/tcp": {}},
				Memory:       1234,
				CPUShares:    99,
				WorkingDir:   "/workingDir",
			},
		},
	}

	fakeEtcdClient, _, storage := setup(t)
	fakeEtcdClient.Data["/imagerepositories/default"] = tools.EtcdResponseWithError{
		R: &etcd.Response{
			Node: &etcd.Node{
				Nodes: []*etcd.Node{
					{
						Value:         runtime.EncodeOrDie(latest.Codec, existingRepo),
						ModifiedIndex: 1,
					},
				},
			},
		},
	}
	fakeEtcdClient.Data["/imagerepositories/default/somerepo"] = tools.EtcdResponseWithError{
		R: &etcd.Response{
			Node: &etcd.Node{
				Value:         runtime.EncodeOrDie(latest.Codec, existingRepo),
				ModifiedIndex: 1,
			},
		},
	}
	fakeEtcdClient.Data["/images/default/existingImage"] = tools.EtcdResponseWithError{
		R: &etcd.Response{
			Node: &etcd.Node{
				Value:         runtime.EncodeOrDie(latest.Codec, existingImage),
				ModifiedIndex: 1,
			},
		},
	}

	mapping := api.ImageStreamMapping{
		Image: *existingImage,
		Tag:   "existingTag",
	}
	_, err := storage.Create(kapi.NewDefaultContext(), &mapping)
	if !errors.IsInvalid(err) {
		t.Fatalf("Unexpected non-error creating mapping: %#v", err)
	}
}
