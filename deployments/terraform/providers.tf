terraform {
  required_version = ">= 1.5.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.23"
    }
  }
}

provider "aws" {
  region = "ap-south-1" # Mumbai Region
  default_tags {
    tags = {
      Project     = "iicpc-benchmarking-platform"
      Environment = "production"
      ManagedBy   = "terraform"
    }
  }
}