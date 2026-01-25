#!/usr/bin/env python3
"""
Chart generator for Celeris benchmark results.
Generates comparison charts for throughput and latency across all server implementations.
"""

import json
import sys
import os
from pathlib import Path
from typing import Dict, List, Any

try:
    import matplotlib.pyplot as plt
    import matplotlib.patches as mpatches
    import numpy as np
except ImportError:
    print("Error: matplotlib and numpy are required")
    print("Install with: pip install matplotlib numpy")
    sys.exit(1)

# Color schemes for different server categories
COLORS = {
    # Baseline servers
    "stdhttp-h1": "#3498db", "stdhttp-h2": "#2980b9", "stdhttp-hybrid": "#1abc9c",
    "fiber-h1": "#e74c3c",
    "iris-h1": "#8e44ad", "iris-h2": "#9b59b6", "iris-hybrid": "#af7ac5",
    "gin-h1": "#e91e63", "gin-h2": "#f06292", "gin-hybrid": "#f48fb1",
    "chi-h1": "#00bcd4", "chi-h2": "#4dd0e1", "chi-hybrid": "#80deea",
    "echo-h1": "#ff5722", "echo-h2": "#ff8a65", "echo-hybrid": "#ffab91",
    # Theoretical - epoll
    "epoll-h1": "#27ae60",
    "epoll-h2": "#229954",
    "epoll-hybrid": "#1e8449",
    # Theoretical - io_uring
    "iouring-h1": "#f39c12",
    "iouring-h2": "#d68910",
    "iouring-hybrid": "#b9770e",
}

CATEGORIES = {
    "HTTP/1.1": [
        "stdhttp-h1", "fiber-h1", "gin-h1", "chi-h1", "echo-h1", "iris-h1", 
        "epoll-h1", "iouring-h1"
    ],
    "HTTP/2": [
        "stdhttp-h2", "gin-h2", "chi-h2", "echo-h2", "iris-h2", 
        "epoll-h2", "iouring-h2"
    ],
    "Hybrid": [
        "stdhttp-hybrid", "gin-hybrid", "chi-hybrid", "echo-hybrid", "iris-hybrid", 
        "epoll-hybrid", "iouring-hybrid"
    ]
}


def load_results(results_dir: str) -> List[Dict[str, Any]]:
    """Load all benchmark result JSON files."""
    results = []
    results_path = Path(results_dir)
    
    for json_file in results_path.glob("benchmark-*.json"):
        with open(json_file) as f:
            data = json.load(f)
            results.append(data)
    
    return results


def generate_throughput_chart(results: Dict[str, Any], benchmark_type: str, output_dir: str):
    """Generate a bar chart comparing throughput across servers for a specific benchmark."""
    
    # Filter results for this benchmark type
    benchmark_results = [r for r in results.get("results", []) 
                        if r.get("benchmark") == benchmark_type and "requests_per_sec" in r]
    
    if not benchmark_results:
        print(f"No results found for benchmark: {benchmark_type}")
        return
    
    # Sort by category and then by requests per second
    servers = []
    reqs_per_sec = []
    colors = []
    
    for category in ["HTTP/1.1", "HTTP/2", "Hybrid"]:
        for server in CATEGORIES.get(category, []):
            for result in benchmark_results:
                if result.get("server") == server:
                    servers.append(server)
                    reqs_per_sec.append(float(result.get("requests_per_sec", 0)))
                    colors.append(COLORS.get(server, "#95a5a6"))
                    break
    
    if not servers:
        return
    
    # Create figure
    fig, ax = plt.subplots(figsize=(14, 8))
    
    x = np.arange(len(servers))
    bars = ax.bar(x, reqs_per_sec, color=colors, edgecolor='white', linewidth=0.7)
    
    # Add value labels on bars
    for bar, val in zip(bars, reqs_per_sec):
        height = bar.get_height()
        ax.annotate(f'{val/1000:.1f}K',
                   xy=(bar.get_x() + bar.get_width() / 2, height),
                   xytext=(0, 3),
                   textcoords="offset points",
                   ha='center', va='bottom', fontsize=9, fontweight='bold')
    
    ax.set_xlabel('Server Implementation', fontsize=12)
    ax.set_ylabel('Requests per Second', fontsize=12)
    ax.set_title(f'Throughput Comparison: {benchmark_type.title()} Benchmark\n'
                f'Architecture: {results.get("architecture", "unknown")}', fontsize=14, fontweight='bold')
    ax.set_xticks(x)
    ax.set_xticklabels(servers, rotation=45, ha='right')
    
    # Add category separators
    # Separators between protocol groups
    # Calculate positions based on group sizes (which are static mostly)
    # H1 group size: 8 -> split at 7.5
    # H2 group size: 7 -> 8 + 7 = 15 -> split at 14.5
    ax.axvline(x=7.5, color='gray', linestyle='--', alpha=0.5)
    ax.axvline(x=14.5, color='gray', linestyle='--', alpha=0.5)
    
    # Legend
    legend_patches = [
        mpatches.Patch(color='#3498db', label='HTTP/1.1'),
        mpatches.Patch(color='#2980b9', label='HTTP/2'),
        mpatches.Patch(color='#1abc9c', label='Hybrid'),
        mpatches.Patch(color='#27ae60', label='Epoll (Ref)'),
        mpatches.Patch(color='#f39c12', label='IoUring (Ref)'),
    ]
    ax.legend(handles=legend_patches, loc='upper right')
    
    ax.set_ylim(bottom=0)
    ax.grid(axis='y', alpha=0.3)
    
    plt.tight_layout()
    
    output_path = Path(output_dir) / f"throughput_{benchmark_type}_{results.get('architecture', 'unknown')}.png"
    plt.savefig(output_path, dpi=150, bbox_inches='tight')
    plt.close()
    print(f"Generated: {output_path}")


def generate_latency_chart(results: Dict[str, Any], output_dir: str):
    """Generate a latency distribution chart for all servers."""
    
    # Filter latency results
    # Filter latency results
    # Go benchmark output includes latency in the main result object, so we verify specific fields exist
    latency_results = [r for r in results.get("results", []) 
                      if r.get("latency")]
    
    if not latency_results:
        print("No latency results found")
        return
    
    # Prepare data
    servers = []
    p50_values = []
    p99_values = []
    p999_values = []
    colors = []
    
    for category in ["HTTP/1.1", "HTTP/2", "Hybrid"]:
        for server in CATEGORIES.get(category, []):
            for result in latency_results:
                if result.get("server") == server:
                    servers.append(server)
                    latency = result.get("latency", {})
                    p50_values.append(parse_latency(latency.get("p50", "0")))
                    p99_values.append(parse_latency(latency.get("p99", "0")))
                    p999_values.append(parse_latency(latency.get("p99.9", "0")))
                    colors.append(COLORS.get(server, "#95a5a6"))
                    break
    
    if not servers:
        return
    
    # Create grouped bar chart
    fig, ax = plt.subplots(figsize=(14, 8))
    
    x = np.arange(len(servers))
    width = 0.25
    
    bars1 = ax.bar(x - width, p50_values, width, label='p50', color='#3498db', alpha=0.8)
    bars2 = ax.bar(x, p99_values, width, label='p99', color='#e74c3c', alpha=0.8)
    bars3 = ax.bar(x + width, p999_values, width, label='p99.9', color='#f39c12', alpha=0.8)
    
    ax.set_xlabel('Server Implementation', fontsize=12)
    ax.set_ylabel('Latency (microseconds)', fontsize=12)
    ax.set_title(f'Latency Distribution Comparison\n'
                f'Architecture: {results.get("architecture", "unknown")}', fontsize=14, fontweight='bold')
    ax.set_xticks(x)
    ax.set_xticklabels(servers, rotation=45, ha='right')
    ax.legend()
    
    ax.set_ylim(bottom=0)
    ax.grid(axis='y', alpha=0.3)
    
    plt.tight_layout()
    
    output_path = Path(output_dir) / f"latency_{results.get('architecture', 'unknown')}.png"
    plt.savefig(output_path, dpi=150, bbox_inches='tight')
    plt.close()
    print(f"Generated: {output_path}")


def parse_latency(latency_str: str) -> float:
    """Parse latency string (e.g., '1.23ms', '456us') to microseconds."""
    if not latency_str:
        return 0.0
    
    latency_str = latency_str.strip()
    
    if latency_str.endswith('ms'):
        return float(latency_str[:-2]) * 1000
    elif latency_str.endswith('us') or latency_str.endswith('µs'):
        return float(latency_str[:-2])
    elif latency_str.endswith('s'):
        return float(latency_str[:-1]) * 1000000
    else:
        try:
            return float(latency_str)
        except ValueError:
            return 0.0


def generate_summary_table(results: Dict[str, Any], output_dir: str):
    """Generate a markdown summary table."""
    
    output_path = Path(output_dir) / f"summary_{results.get('architecture', 'unknown')}.md"
    
    with open(output_path, 'w') as f:
        f.write(f"# Benchmark Results: {results.get('architecture', 'unknown')}\n\n")
        f.write(f"**Timestamp:** {results.get('timestamp', 'N/A')}\n\n")
        
        config = results.get('config', {})
        f.write(f"**Configuration:**\n")
        f.write(f"- Duration: {config.get('duration', 'N/A')}\n")
        f.write(f"- Connections: {config.get('connections', 'N/A')}\n")
        f.write(f"- Workers: {config.get('workers', 'N/A')}\n\n")
        
        # Throughput table
        f.write("## Throughput (requests/sec)\n\n")
        f.write("| Server | Simple | JSON | Path | Big Request |\n")
        f.write("|--------|--------|------|------|-------------|\n")
        
        benchmark_types = ["simple", "json", "path", "big-request"]
        all_servers = CATEGORIES["HTTP/1.1"] + CATEGORIES["HTTP/2"] + CATEGORIES["Hybrid"]
        
        # First pass: collect servers that have at least one result
        servers_with_data = set()
        for result in results.get("results", []):
            servers_with_data.add(result.get("server"))
        
        for server in all_servers:
            # Skip servers with no data
            if server not in servers_with_data:
                continue
                
            row = [server]
            for bench_type in benchmark_types:
                for result in results.get("results", []):
                    if result.get("server") == server and result.get("benchmark") == bench_type:
                        rps = result.get("requests_per_sec", 0)
                        row.append(f"{float(rps):,.0f}")
                        break
                else:
                    row.append("-")
            f.write("| " + " | ".join(row) + " |\n")
        
        f.write("\n")
    
    print(f"Generated: {output_path}")


def main():
    if len(sys.argv) < 2:
        print("Usage: generate_charts.py <results_directory> [output_directory]")
        sys.exit(1)
    
    results_dir = sys.argv[1]
    output_dir = sys.argv[2] if len(sys.argv) > 2 else results_dir
    
    # Create output directory
    Path(output_dir).mkdir(parents=True, exist_ok=True)
    
    # Load results
    all_results = load_results(results_dir)
    
    if not all_results:
        print(f"No benchmark results found in: {results_dir}")
        sys.exit(1)
    
    print(f"Found {len(all_results)} result file(s)")
    
    # Generate charts for each result file
    for results in all_results:
        print(f"\nProcessing: {results.get('architecture', 'unknown')} @ {results.get('timestamp', 'unknown')}")
        
        # Generate throughput charts for each benchmark type
        for bench_type in ["simple", "json", "path", "big-request"]:
            generate_throughput_chart(results, bench_type, output_dir)
        
        # Generate latency chart
        generate_latency_chart(results, output_dir)
        
        # Generate summary table
        generate_summary_table(results, output_dir)
    
    print("\nChart generation complete!")


if __name__ == "__main__":
    main()
