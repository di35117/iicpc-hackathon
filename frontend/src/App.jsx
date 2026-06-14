import React, { useState, useEffect } from "react";

function App() {
  // 1. URL-SYNCED STATE INITIALIZATION
  // Instead of starting null, we check the URL parameters on first load.
  // If ?run_id=xyz exists (e.g., after a refresh), React immediately resumes tracking it.
  const [runId, setRunId] = useState(() => {
    const params = new URLSearchParams(window.location.search);
    return params.get("run_id") || null;
  });

  const [scoreData, setScoreData] = useState(null);
  const [isTesting, setIsTesting] = useState(false);
  const [uploadMessage, setUploadMessage] = useState("");
  const [submissionId, setSubmissionId] = useState(null);

  // --- 2. THE POLLING ENGINE ---
  useEffect(() => {
    // If no run is active, do nothing.
    if (!runId) return;

    setIsTesting(true);
    let pollInterval;

    const fetchScore = async () => {
      try {
        const res = await fetch(`http://localhost:8084/score/${runId}`);
        if (res.ok) {
          const data = await res.json();
          setScoreData(data);

          // Optional: Stop polling if you have a "completed" flag in your payload
          // if (data.status === 'completed') clearInterval(pollInterval);
        } else if (res.status === 404) {
          console.log("Awaiting TimescaleDB aggregation...");
        }
      } catch (err) {
        console.error("Leaderboard API unreachable or offline.");
      }
    };

    // Fire immediately, then poll every 2 seconds
    fetchScore();
    pollInterval = setInterval(fetchScore, 2000);

    // Cleanup interval on unmount or when runId changes
    return () => clearInterval(pollInterval);
  }, [runId]); // Dependency array ensures polling binds to the current URL ID

  // --- 3. UPLOAD HANDLER ---
  const handleUpload = async (e) => {
    const file = e.target.files[0];
    if (!file) return;

    const formData = new FormData();
    formData.append("source", file);

    setUploadMessage("Uploading & Compiling...");
    try {
      const res = await fetch("http://localhost:8081/submit", {
        method: "POST",
        body: formData,
      });
      const data = await res.json();

      if (res.ok) {
        setSubmissionId(data.submission_id);
        setUploadMessage(`Sandboxed successfully. ID: ${data.submission_id}`);
      } else {
        setUploadMessage(`Build failed: ${data.message || "Unknown error"}`);
      }
    } catch (err) {
      setUploadMessage("Connection error to upload-api.");
    }
  };

  // --- 4. STRESS TEST HANDLER ---
  const initiateStressTest = async () => {
    if (!submissionId) {
      alert("Please upload a submission first.");
      return;
    }

    setIsTesting(true);
    setScoreData(null); // Clear old scores

    try {
      const res = await fetch(`http://localhost:8081/run/${submissionId}`, {
        method: "POST",
      });
      const data = await res.json();

      if (res.ok) {
        const newRunId = data.run_id;

        // UPDATE STATE
        setRunId(newRunId);

        // SILENT URL UPDATE
        // This pushes the new run_id into the browser's address bar without reloading the page.
        // If the user refreshes now, the lazy init at the top of the file catches it.
        window.history.pushState({}, "", `?run_id=${newRunId}`);
      } else {
        alert(`Failed to start run: ${data.message}`);
        setIsTesting(false);
      }
    } catch (err) {
      alert("Failed to reach orchestrator.");
      setIsTesting(false);
    }
  };

  return (
    <div className="min-h-screen bg-[#0b0f19] text-white font-mono p-8">
      {/* HEADER SECTION */}
      <header className="mb-12 border-b border-slate-800 pb-6 flex justify-between items-end">
        <div>
          <h1 className="text-4xl font-bold tracking-wider mb-2">
            <span className="text-red-500">IICPC</span> COMMAND CENTER
          </h1>
          <p className="text-slate-400 text-sm tracking-widest">
            DISTRIBUTED BENCHMARKING PLATFORM • LIVE SCORING MATRIX
          </p>
        </div>

        <div className="flex gap-4">
          <label className="cursor-pointer border border-[#00f0ff] text-[#00f0ff] hover:bg-[#00f0ff]/10 px-6 py-2 transition-colors">
            [ + ] UPLOAD SUBMISSION
            <input type="file" className="hidden" onChange={handleUpload} />
          </label>

          <button
            onClick={initiateStressTest}
            disabled={!submissionId}
            className={`px-6 py-2 border transition-colors flex items-center gap-2
              ${
                submissionId
                  ? "border-red-500 text-red-500 hover:bg-red-500/10"
                  : "border-slate-700 text-slate-700 cursor-not-allowed"
              }`}
          >
            ⚡ INITIATE STRESS TEST
          </button>
        </div>
      </header>

      {/* STATUS BAR */}
      {uploadMessage && (
        <div className="mb-8 p-3 border border-slate-800 text-sm text-[#00f0ff]">
          {uploadMessage}
        </div>
      )}

      {/* MAIN DASHBOARD */}
      <main className="border border-slate-800 border-dashed rounded-lg p-1 min-h-[400px] flex flex-col items-center justify-center relative">
        {/* Loading State */}
        {isTesting && !scoreData && (
          <div className="flex flex-col items-center gap-4 text-slate-400">
            <div className="w-12 h-12 border-4 border-slate-800 border-t-[#00f0ff] rounded-full animate-spin"></div>
            <p>Awaiting telemetry data stream from bot fleet...</p>
            <p className="text-xs text-slate-600">Run ID: {runId}</p>
          </div>
        )}

        {/* Scorecard State */}
        {scoreData && (
          <div className="w-full h-full bg-[#0f1423] border-l-4 border-[#00f0ff] p-8">
            <div className="flex justify-between items-start mb-12">
              <div>
                <h2 className="text-2xl font-bold text-slate-300">
                  RANK 1 //{" "}
                  <span className="text-white">
                    Submission: {scoreData.submission_id.substring(0, 8)}
                  </span>
                </h2>
                <p className="text-slate-500 text-sm mt-2">
                  Run ID: {scoreData.run_id}
                </p>
              </div>
              <div className="text-right">
                <p className="text-slate-500 text-xs tracking-widest mb-1">
                  COMPOSITE SCORE
                </p>
                <p className="text-5xl font-bold text-yellow-400">
                  {scoreData.composite_score.toFixed(3)}
                </p>
              </div>
            </div>

            <div className="grid grid-cols-3 gap-6">
              {/* TPS */}
              <div className="border border-slate-800 bg-[#0b0f19] p-6">
                <div className="flex justify-between text-xs mb-4">
                  <span className="text-slate-500">PEAK THROUGHPUT</span>
                  <span className="text-green-500">TPS</span>
                </div>
                <p className="text-4xl font-bold text-white">
                  {scoreData.peak_tps.toLocaleString()}
                </p>
              </div>

              {/* Latency */}
              <div className="border border-slate-800 bg-[#0b0f19] p-6">
                <div className="flex justify-between text-xs mb-4">
                  <span className="text-slate-500">TAIL LATENCY (P99)</span>
                  <span className="text-red-500">MS / MS</span>
                </div>
                <p className="text-4xl font-bold text-white">
                  {(scoreData.avg_p99_latency_us / 1000).toFixed(2)}{" "}
                  <span className="text-xl text-slate-500">ms</span>
                </p>
              </div>

              {/* Integrity */}
              <div className="border border-red-900/30 bg-[#1a0f14] p-6">
                <div className="flex justify-between text-xs mb-4">
                  <span className="text-slate-500">ALGORITHMIC INTEGRITY</span>
                  <span className="text-red-500">FIFO</span>
                </div>
                <p className="text-4xl font-bold text-red-500">
                  {scoreData.integrity_percentage.toFixed(2)}{" "}
                  <span className="text-xl">%</span>
                </p>
              </div>
            </div>
          </div>
        )}
      </main>
    </div>
  );
}

export default App;
