package cuex

import (
	"github.com/kubevela/pkg/cue/cuex"

	"github.com/oam-dev/kubevela/pkg/cue/cuex/providers/config"
)

// KubeVelaDefaultCompiler compiler for cuex to compile
var KubeVelaDefaultCompiler = cuex.NewCompilerWithInternalPackages(
	config.Package,
)
