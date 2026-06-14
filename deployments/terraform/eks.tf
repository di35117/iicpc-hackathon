module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 20.0"

  cluster_name    = "iicpc-benchmarking-cluster"
  cluster_version = "1.30"

  vpc_id                   = module.vpc.vpc_id
  subnet_ids               = module.vpc.private_subnets
  cluster_endpoint_public_access = true

  eks_managed_node_groups = {
    # General nodes for APIs, TimescaleDB, and Redpanda
    core_infrastructure = {
      min_size       = 2
      max_size       = 5
      desired_size   = 2
      instance_types = ["m6i.xlarge"]
      capacity_type  = "ON_DEMAND"
    }

    # High-CPU nodes exclusively for generating massive WebSocket load
    bot_fleet_workers = {
      min_size       = 3
      max_size       = 15
      desired_size   = 5
      instance_types = ["c6i.2xlarge"]
      capacity_type  = "SPOT" # Cost optimization for stateless workers
      
      labels = {
        workload = "high-throughput-generator"
      }
      taints = [{
        key    = "dedicated"
        value  = "bot-fleet"
        effect = "NO_SCHEDULE"
      }]
    }
  }
}