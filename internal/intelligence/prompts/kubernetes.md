# Reading Kubernetes evidence

- Conditions: a condition's status is True, False or Unknown. Ready,
  Available, Healthy, Synced and similar are good when True; Stalled,
  Degraded, MemoryPressure and other conditions named for a problem are bad
  when True; Progressing and Reconciling mean work is under way. A
  condition's reason and message are the controller's own words, and its age
  says when the status last changed, not when it was last checked.
- Generations: metadata.generation counts changes to an object's spec, and
  status.observedGeneration is the generation the controller last acted on.
  When the observed generation is behind, the controller has not caught up
  with the latest change, and the conditions describe an older spec.
- Owners: objects own others through ownerReferences: a Deployment owns
  ReplicaSets, which own Pods; a CronJob owns Jobs; an operator's resource
  owns what it creates. A problem usually starts at the bottom of the chain,
  in a pod or a container, and shows at the top.
- Events: Kubernetes keeps events for about an hour by default and merges
  repeats into one with a count. An event's absence does not mean nothing
  happened, and an old problem can have left none.
- Containers: CrashLoopBackOff means a container keeps exiting and the kubelet
  waits longer before each restart; the last exit code and the log of the
  previous run say why. Exit code 137 is a kill, often for memory
  (OOMKilled); 143 is a termination; 1 and other small codes are the
  application's own failure. ImagePullBackOff is an image that cannot be
  pulled: a wrong name or tag, or no access to the registry.
- Scheduling: "0/3 nodes are available" lists why each node refused the pod:
  resources, taints, affinity, volume zones. A PersistentVolumeClaim stays
  Pending until a volume is bound; with WaitForFirstConsumer it waits for a
  pod to be scheduled first.
- tug's own diagnosis and findings are evidence too, with tug's suggested
  next step; agree with them or say why not.
