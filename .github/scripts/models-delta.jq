# Structural diff between two pkg/modelsdev/snapshot.json documents.
#
# Invoked as:
#   jq -n --slurpfile old OLD.json --slurpfile new NEW.json -f models-delta.jq
#
# Both snapshots are map[providerID]{models: map[modelID]Model}. Flatten each
# to a single map keyed by "provider/model" so added/removed/changed models
# can be diffed by key regardless of which provider they live under.
def flatten_models:
  [
    to_entries[] as $p
    | ($p.value.models // {}) | to_entries[] as $m
    | { key: ($p.key + "/" + $m.key), value: $m.value }
  ] | from_entries;

($old[0] | flatten_models) as $o
| ($new[0] | flatten_models) as $n
| {
    added: [
      $n | keys_unsorted[] | select($o[.] == null) as $id
      | { id: $id, model: $n[$id] }
    ],
    removed: [
      $o | keys_unsorted[] | select($n[.] == null) as $id
      | { id: $id, model: $o[$id] }
    ],
    changed: [
      $n | keys_unsorted[] | select($o[.] != null and $o[.] != $n[.]) as $id
      | {
          id: $id,
          fields: [
            (($o[$id] + $n[$id]) | keys_unsorted[]) as $f
            | select($o[$id][$f] != $n[$id][$f])
            | { field: $f, before: $o[$id][$f], after: $n[$id][$f] }
          ]
        }
    ]
  }
