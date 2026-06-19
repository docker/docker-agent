namespace RAGPrefetch

def ExactHit {Query : Type} (cache : Query -> Prop) (query : Query) : Prop :=
  cache query

def TopologyHit {Query : Type} (cache : Query -> Prop) (related : Query -> Query -> Prop) (query : Query) : Prop :=
  exists cached, cache cached /\ related query cached

theorem exact_hit_is_topology_hit_when_related_self
    {Query : Type}
    {cache : Query -> Prop}
    {related : Query -> Query -> Prop}
    {query : Query}
    (hit : ExactHit cache query)
    (selfRelated : related query query) :
    TopologyHit cache related query := by
  exists query

theorem topology_hit_can_strictly_extend_exact_hit
    {Query : Type}
    {cache : Query -> Prop}
    {related : Query -> Query -> Prop}
    {query cached : Query}
    (cachedHit : cache cached)
    (topologyRelated : related query cached)
    (exactMiss : Not (cache query)) :
    TopologyHit cache related query /\ Not (ExactHit cache query) := by
  constructor
  · exists cached
  · exact exactMiss

end RAGPrefetch
