import React, { useState, useEffect, useRef } from "react";

export default function Leaderboard() {
  const [scores, setScores] = useState([]);
  const [isLive, setIsLive] = useState(true);
  const [isDeploying, setIsDeploying] = useState(false);
  const [uploadStatus, setUploadStatus] = useState("");

  // Dynamic State tracking
  const [submissionId, setSubmissionId] = useState(null);
  const [activeRunId, setActiveRunId] = useState(
    "9dadf7fa-4169-4c0c-892d-a568db6ed9c4",
  ); // Fallback to your first run

  const fileInputRef = useRef(null);

  // Poll the Leaderboard API based on the currently active run
  useEffect(() => {
    const fetchScores = async () => {
      if (!isLive || !activeRunId) return;
      try {
        const res = await fetch(`http://localhost:8084/score/${activeRunId}`);
        if (res.ok) {
          const data = await res.json();
          setScores([data]);
        } else if (res.status === 404) {
          // If the run just started, metrics might not exist in the DB for the first 1-2 seconds
          setScores([]);
        }
      } catch (error) {
        console.error("Failed to fetch telemetry:", error);
      }
    };

    fetchScores();
    const interval = setInterval(fetchScores, 2000);
    return () => clearInterval(interval);
  }, [isLive, activeRunId]);

  // Step 1: Upload Code to the Orchestrator
  const handleFileUpload = async (e) => {
    const file = e.target.files[0];
    if (!file) return;

    setUploadStatus("Uploading & Sandboxing...");

    const formData = new FormData();
    formData.append("source", file); // Matches r.FormFile("source") in the new backend

    try {
      const res = await fetch("http://localhost:8081/submit", {
        method: "POST",
        body: formData,
      });

      const data = await res.json();
      if (res.ok) {
        setSubmissionId(data.submission_id);
        setUploadStatus(`Success: ${data.message}`);
        setTimeout(() => setUploadStatus(""), 4000);
      } else {
        setUploadStatus(`Error: ${data.message || "Upload failed"}`);
      }
    } catch (error) {
      console.error("Upload error:", error);
      setUploadStatus("Connection error. Is upload-api running?");
    }
  };

  // Step 2: Trigger Deployment & Load Test via the Orchestrator
  const handleInitiateStressTest = async () => {
    if (!submissionId) {
      alert("Please upload a valid submission first!");
      return;
    }

    setIsDeploying(true);
    setScores([]); // Clear the board for the new run

    try {
      const res = await fetch(`http://localhost:8081/run/${submissionId}`, {
        method: "POST",
      });

      const data = await res.json();
      if (res.ok) {
        // Instantly switch the leaderboard to track the new bots
        setActiveRunId(data.run_id);
      } else {
        alert("Failed to start load test.");
      }
      setTimeout(() => setIsDeploying(false), 2000);
    } catch (error) {
      console.error("Failed to trigger fleet:", error);
      setIsDeploying(false);
    }
  };

  return (
    <div className="min-h-screen bg-slate-950 text-cyan-400 p-8 font-mono tracking-tight selection:bg-cyan-900">
      {/* Control Center Header */}
      <div className="max-w-6xl mx-auto mb-10 flex flex-col md:flex-row items-start md:items-end justify-between border-b-2 border-slate-800 pb-6 gap-6">
        <div>
          <h1 className="text-4xl font-extrabold text-white tracking-widest flex items-center gap-3">
            <span className="text-red-500">IICPC</span> COMMAND CENTER
          </h1>
          <p className="text-slate-500 mt-2 text-sm uppercase tracking-widest">
            Distributed Benchmarking Platform • Live Scoring Matrix
          </p>

          {/* Dynamic Status Badges */}
          <div className="mt-4 flex gap-4">
            {submissionId && (
              <span className="bg-cyan-900/30 border border-cyan-800 text-cyan-400 px-2 py-1 rounded text-xs">
                Active Submission: {submissionId.split("-")[0]}
              </span>
            )}
            {uploadStatus && (
              <span
                className={`px-2 py-1 rounded text-xs border ${uploadStatus.includes("Error") || uploadStatus.includes("failed") ? "bg-red-900/30 border-red-800 text-red-500" : "bg-yellow-900/30 border-yellow-800 text-yellow-400 animate-pulse"}`}
              >
                {uploadStatus}
              </span>
            )}
          </div>
        </div>

        {/* Action Buttons */}
        <div className="flex items-center gap-4">
          <input
            type="file"
            ref={fileInputRef}
            className="hidden"
            onChange={handleFileUpload}
            accept=".cpp,.rs,.go,.c,.zip,.tar.gz"
          />
          <button
            className="px-6 py-2 bg-slate-900 hover:bg-slate-800 border border-cyan-800 hover:border-cyan-500 rounded text-cyan-400 font-bold transition-all text-sm"
            onClick={() => fileInputRef.current.click()}
          >
            [ + ] UPLOAD SUBMISSION
          </button>

          <button
            onClick={handleInitiateStressTest}
            disabled={isDeploying || !submissionId}
            className={`px-6 py-2 border rounded font-bold transition-all text-sm flex items-center gap-2
              ${
                !submissionId
                  ? "bg-slate-900 border-slate-800 text-slate-600 cursor-not-allowed"
                  : isDeploying
                    ? "bg-yellow-900/50 border-yellow-700 text-yellow-500 cursor-not-allowed"
                    : "bg-red-900/50 border-red-700 text-red-500 hover:bg-red-900 hover:text-white"
              }`}
          >
            {isDeploying ? "DEPLOYING FLEET..." : "⚡ INITIATE STRESS TEST"}
          </button>
        </div>
      </div>

      {/* Leaderboard Grid */}
      <div className="max-w-6xl mx-auto grid gap-6">
        {scores.map((score, index) => (
          <div
            key={score.run_id}
            className="relative bg-slate-900 border border-slate-700 rounded-xl p-6 overflow-hidden shadow-2xl transition-all hover:border-cyan-500"
          >
            <div className="absolute top-0 right-0 w-32 h-32 bg-cyan-900/10 rounded-bl-full -z-10"></div>
            <div className="absolute top-0 left-0 w-2 h-full bg-gradient-to-b from-cyan-500 to-blue-600"></div>

            <div className="flex justify-between items-start mb-8 pl-4">
              <div>
                <h2 className="text-2xl font-bold text-white flex items-center gap-2">
                  <span className="text-slate-500 text-lg">
                    RANK {index + 1} //
                  </span>
                  Submission: {score.submission_id.split("-")[0].toUpperCase()}
                </h2>
                <p className="text-slate-500 text-xs mt-1 font-mono">
                  Run ID: {score.run_id}
                </p>
              </div>
              <div className="text-right">
                <span className="block text-xs text-slate-400 uppercase tracking-widest mb-1">
                  Composite Score
                </span>
                <span className="text-4xl font-black text-yellow-400 drop-shadow-[0_0_10px_rgba(250,204,21,0.3)]">
                  {score.composite_score.toFixed(3)}
                </span>
              </div>
            </div>

            <div className="grid grid-cols-1 md:grid-cols-3 gap-4 pl-4">
              <div className="bg-slate-950 p-5 rounded-lg border border-slate-800 flex flex-col justify-between">
                <span className="text-xs text-slate-400 uppercase tracking-widest mb-2 flex justify-between">
                  <span>Peak Throughput</span>
                  <span className="text-green-500">TPS</span>
                </span>
                <span className="text-3xl font-bold text-white">
                  {score.peak_tps.toLocaleString()}
                </span>
              </div>

              <div className="bg-slate-950 p-5 rounded-lg border border-slate-800 flex flex-col justify-between">
                <span className="text-xs text-slate-400 uppercase tracking-widest mb-2 flex justify-between">
                  <span>Tail Latency (p99)</span>
                  <span className="text-red-400">μs / ms</span>
                </span>
                <span className="text-3xl font-bold text-white">
                  {(score.avg_p99_latency_us / 1000).toFixed(2)}{" "}
                  <span className="text-lg text-slate-500">ms</span>
                </span>
              </div>

              <div
                className={`bg-slate-950 p-5 rounded-lg border flex flex-col justify-between ${score.integrity_percentage < 100 ? "border-red-900/50" : "border-slate-800"}`}
              >
                <span className="text-xs text-slate-400 uppercase tracking-widest mb-2 flex justify-between">
                  <span>Algorithmic Integrity</span>
                  <span
                    className={
                      score.integrity_percentage < 100
                        ? "text-red-500"
                        : "text-cyan-500"
                    }
                  >
                    FIFO
                  </span>
                </span>
                <span
                  className={`text-3xl font-bold ${score.integrity_percentage < 100 ? "text-red-500" : "text-white"}`}
                >
                  {score.integrity_percentage.toFixed(2)}{" "}
                  <span className="text-lg opacity-50">%</span>
                </span>
              </div>
            </div>
          </div>
        ))}

        {scores.length === 0 && (
          <div className="text-center py-20 border-2 border-dashed border-slate-800 rounded-xl">
            <div className="w-12 h-12 border-4 border-cyan-900 border-t-cyan-500 rounded-full animate-spin mx-auto mb-4"></div>
            <p className="text-slate-500 font-mono animate-pulse">
              Awaiting telemetry data stream from bot fleet...
            </p>
          </div>
        )}
      </div>
    </div>
  );
}
