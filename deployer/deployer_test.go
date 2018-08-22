package deployer

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/stretchr/testify/assert"
)

func TestOverrideImages(t *testing.T) {
	type args struct {
		cds  []*ecs.ContainerDefinition
		tags []string
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
		wantCDs []*ecs.ContainerDefinition
	}{{
		name: "unamed tag with single container definition",
		args: args{
			cds:  containers("myapp", "myimage:1"),
			tags: []string{"2"},
		},
		wantCDs: containers("myapp", "myimage:2"),
	}, {
		name: "named tag with single container definition",
		args: args{
			cds:  containers("myapp", "myimage:1"),
			tags: []string{"myapp=2"},
		},
		wantCDs: containers("myapp", "myimage:2"),
	}, {
		name: "named tag with multiple container definition",
		args: args{
			cds:  containers("myapp", "myimage:1", "yourapp", "yourimage:1"),
			tags: []string{"yourapp=2"},
		},
		wantCDs: containers("myapp", "myimage:1", "yourapp", "yourimage:2"),
	}, {
		name: "multiple named tags with multiple container definition",
		args: args{
			cds:  containers("myapp", "myimage:1", "yourapp", "yourimage:1"),
			tags: []string{"yourapp=2", "myapp=5"},
		},
		wantCDs: containers("myapp", "myimage:5", "yourapp", "yourimage:2"),
	}, {
		name: "unnamed tag with multiple container definition",
		args: args{
			cds:  containers("myapp", "myimage:1", "yourapp", "yourimage:1"),
			tags: []string{"2"},
		},
		wantErr: true,
	}, {
		name: "non-matching container name with multiple container definition",
		args: args{
			cds:  containers("myapp", "myimage:1", "yourapp", "yourimage:1"),
			tags: []string{"yourapp=2", "foobar=5"},
		},
		wantErr: true,
	}, {
		name: "not all specs have container name",
		args: args{
			cds:  containers("myapp", "myimage:1", "yourapp", "yourimage:1"),
			tags: []string{"yourapp=2", "5"},
		},
		wantErr: true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := OverrideImages(&ecs.TaskDefinition{ContainerDefinitions: tt.args.cds}, &Request{Tags: tt.args.tags}); (err != nil) != tt.wantErr {
				t.Errorf("OverrideImages() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				assert.Equal(t, tt.wantCDs, tt.args.cds)
			}
		})
	}
}

func containers(args ...string) []*ecs.ContainerDefinition {
	var cds []*ecs.ContainerDefinition
	for i := 0; i < len(args); i += 2 {
		cds = append(cds, &ecs.ContainerDefinition{
			Name:  aws.String(args[i]),
			Image: aws.String(args[i+1]),
		})
	}
	return cds
}
