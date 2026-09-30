package spec

import (
	"cmp"
	"maps"
	"slices"
)

// Merge combines a user and a project spec into the effective desired state:
// project values win at the same level and at the project global level,
// sources and packages keep project order first, a shared package id is
// replaced wholesale and unknown fields merge with project priority.
// Merge(nil, nil) returns New(). Both inputs must be valid.
func Merge(user, project *Spec) (*Spec, error) {
	if user == nil && project == nil {
		return New(), nil
	}

	if err := validateOptional(user); err != nil {
		return nil, err
	}

	if err := validateOptional(project); err != nil {
		return nil, err
	}

	switch {
	case project == nil:
		return cloneSpec(user), nil
	case user == nil:
		return cloneSpec(project), nil
	}

	return &Spec{
		Schema:    cmp.Or(project.Schema, user.Schema),
		Defaults:  mergeDefaults(user.Defaults, project.Defaults),
		Propagate: mergePropagate(user.Propagate, project.Propagate),
		Sources:   mergeSources(user.Sources, project.Sources),
		Packages:  mergePackages(user.Packages, project.Packages),
		raw:       mergeMaps(user.raw, project.raw),
	}, nil
}

func validateOptional(spec *Spec) error {
	if spec == nil {
		return nil
	}

	return validate(spec, "")
}

func mergeDefaults(user, project Defaults) Defaults {
	return Defaults{
		Hooks:    cmp.Or(project.Hooks, user.Hooks),
		Cooldown: cmp.Or(project.Cooldown, user.Cooldown),
		raw:      mergeMaps(user.raw, project.raw),
	}
}

func mergePropagate(user, project Propagate) Propagate {
	merged := Propagate{
		Install:     cmp.Or(project.Install, user.Install),
		Remove:      cmp.Or(project.Remove, user.Remove),
		Disable:     cmp.Or(project.Disable, user.Disable),
		Enable:      cmp.Or(project.Enable, user.Enable),
		Update:      cmp.Or(project.Update, user.Update),
		Adopt:       cmp.Or(project.Adopt, user.Adopt),
		Marketplace: cmp.Or(project.Marketplace, user.Marketplace),
		raw:         mergeMaps(user.raw, project.raw),
	}

	merged.Kind = mergePolicyLevels(user.Kind, project.Kind, user, project)
	merged.Host = mergePolicyLevels(user.Host, project.Host, user, project)

	return merged
}

// mergePolicyLevels materializes scope precedence for kind and host override
// tables (OQ-T0.5.1): at level L, project[L][E] wins, else project.global[E],
// else user[L][E], else user.global[E].
func mergePolicyLevels(user, project map[string]Propagate, userGlobal, projectGlobal Propagate) map[string]Propagate {
	if len(user) == 0 && len(project) == 0 {
		return nil
	}

	merged := make(map[string]Propagate, len(user)+len(project))

	for _, name := range unionKeys(user, project) {
		userPolicy := user[name]
		projectPolicy := project[name]

		merged[name] = Propagate{
			Install:     cmp.Or(projectPolicy.Install, projectGlobal.Install, userPolicy.Install, userGlobal.Install),
			Remove:      cmp.Or(projectPolicy.Remove, projectGlobal.Remove, userPolicy.Remove, userGlobal.Remove),
			Disable:     cmp.Or(projectPolicy.Disable, projectGlobal.Disable, userPolicy.Disable, userGlobal.Disable),
			Enable:      cmp.Or(projectPolicy.Enable, projectGlobal.Enable, userPolicy.Enable, userGlobal.Enable),
			Update:      cmp.Or(projectPolicy.Update, projectGlobal.Update, userPolicy.Update, userGlobal.Update),
			Adopt:       cmp.Or(projectPolicy.Adopt, projectGlobal.Adopt, userPolicy.Adopt, userGlobal.Adopt),
			Marketplace: cmp.Or(projectPolicy.Marketplace, projectGlobal.Marketplace, userPolicy.Marketplace, userGlobal.Marketplace),
			raw:         mergeMaps(userPolicy.raw, projectPolicy.raw),
		}
	}

	return merged
}

func unionKeys(user, project map[string]Propagate) []string {
	keys := make(map[string]struct{}, len(user)+len(project))

	for name := range user {
		keys[name] = struct{}{}
	}

	for name := range project {
		keys[name] = struct{}{}
	}

	return slices.Sorted(maps.Keys(keys))
}

func mergeSources(user, project []Source) []Source {
	return mergeEntries(user, project, func(src Source) string {
		return src.Name
	}, cloneSource)
}

func mergePackages(user, project []Package) []Package {
	return mergeEntries(user, project, func(pkg Package) string {
		return pkg.ID
	}, clonePackage)
}

// mergeEntries merges two ordered lists by identity key: entries of over come
// first, then base entries whose key is not taken yet. Every kept entry is
// cloned.
func mergeEntries[T any](base, over []T, key func(T) string, clone func(T) T) []T {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}

	merged := make([]T, 0, len(base)+len(over))
	seen := make(map[string]struct{}, len(over))

	for _, item := range over {
		merged = append(merged, clone(item))
		seen[key(item)] = struct{}{}
	}

	for _, item := range base {
		if _, duplicate := seen[key(item)]; duplicate {
			continue
		}

		merged = append(merged, clone(item))
		seen[key(item)] = struct{}{}
	}

	return merged
}

// mergeMaps merges two unknown-field trees; over wins per key, tables merge
// recursively.
func mergeMaps(base, over map[string]any) map[string]any {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}

	merged := make(map[string]any, len(base)+len(over))
	maps.Copy(merged, base)

	for key, value := range over {
		existing, ok := merged[key]
		if !ok {
			merged[key] = value

			continue
		}

		existingTable, okExisting := existing.(map[string]any)
		valueTable, okValue := value.(map[string]any)

		if okExisting && okValue {
			merged[key] = mergeMaps(existingTable, valueTable)

			continue
		}

		merged[key] = value
	}

	return merged
}

func cloneSpec(spec *Spec) *Spec {
	if spec == nil {
		return nil
	}

	return &Spec{
		Schema:    spec.Schema,
		Defaults:  cloneDefaults(spec.Defaults),
		Propagate: clonePropagate(spec.Propagate),
		Sources:   cloneSources(spec.Sources),
		Packages:  clonePackages(spec.Packages),
		raw:       maps.Clone(spec.raw),
		src:       slices.Clone(spec.src),
		bom:       spec.bom,
	}
}

func cloneDefaults(defaults Defaults) Defaults {
	cloned := defaults
	cloned.raw = maps.Clone(defaults.raw)

	return cloned
}

func clonePropagate(policy Propagate) Propagate {
	cloned := policy
	cloned.raw = maps.Clone(policy.raw)
	cloned.Kind = clonePolicyMap(policy.Kind)
	cloned.Host = clonePolicyMap(policy.Host)

	return cloned
}

func clonePolicyMap(policies map[string]Propagate) map[string]Propagate {
	if policies == nil {
		return nil
	}

	cloned := make(map[string]Propagate, len(policies))
	for name, policy := range policies {
		cloned[name] = clonePropagate(policy)
	}

	return cloned
}

func cloneSources(sources []Source) []Source {
	if sources == nil {
		return nil
	}

	cloned := make([]Source, len(sources))
	for i, src := range sources {
		cloned[i] = cloneSource(src)
	}

	return cloned
}

func cloneSource(src Source) Source {
	cloned := src
	cloned.raw = maps.Clone(src.raw)

	return cloned
}

func clonePackages(packages []Package) []Package {
	if packages == nil {
		return nil
	}

	cloned := make([]Package, len(packages))
	for i, pkg := range packages {
		cloned[i] = clonePackage(pkg)
	}

	return cloned
}

func clonePackage(pkg Package) Package {
	cloned := pkg
	cloned.Except = slices.Clone(pkg.Except)
	cloned.Env = maps.Clone(pkg.Env)
	cloned.raw = maps.Clone(pkg.raw)

	if pkg.Propagate != nil {
		propagate := clonePropagate(*pkg.Propagate)
		cloned.Propagate = &propagate
	}

	return cloned
}
