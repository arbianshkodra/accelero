package service

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockClient is a mock implementation of the Docker client
type MockClient struct {
	mock.Mock
}

func (m *MockClient) ContainerList(ctx context.Context, options container.ListOptions) ([]types.Container, error) {
	args := m.Called(ctx, options)
	return args.Get(0).([]types.Container), args.Error(1)
}

func TestAreContainersRunning(t *testing.T) {
	ctx := context.Background()
	serviceName := "test_service"

	mockCli := new(MockClient)
	mockContainers := []types.Container{
		{
			Names: []string{"/test_service_1"},
		},
	}

	mockCli.On("ContainerList", ctx, mock.Anything).Return(mockContainers, nil)

	running, err := AreContainersRunning(mockCli, serviceName)
	assert.NoError(t, err)
	assert.True(t, running)

	mockCli.AssertExpectations(t)
}
