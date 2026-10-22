;;; guix-sync-commit.scm -- Find the latest Guix commit with ready substitutes
;;; Run with: guix repl -- guix-sync-commit.scm [OPTIONS] [SPEC...]

(use-modules (guix ci)
             (guix utils)
             (srfi srfi-1)
             (srfi srfi-9)
             (srfi srfi-11)
             (srfi srfi-19)
             (ice-9 match)
             (ice-9 format))

(define %default-server "https://ci.guix.gnu.org")
(define %default-limit 40)
(define %pending-statuses '(submitted scheduled started))
(define %failed-statuses '(failed failed-dependency failed-other canceled))

(define (show-help)
  (display "Usage: guix-sync-commit [OPTION]... [SPEC]...
Find the newest Guix commit whose substitutes are ready on ci.guix.gnu.org.

Modes:
  (no SPEC)             whole-evaluation mode: checks if all builds for SYSTEM are finished
  SPEC...               package mode: e.g. emacs git gcc-toolchain@14 bind:utils

Options:
  -s, --system=SYSTEM   target system (default: %current-system)
  -n, --limit=N         number of master evaluations to scan (default: 40)
      --max-pending=N   whole-eval mode: tolerate up to N pending builds (default: 0)
      --allow-failed    package mode: failed builds do not block
      --no-require-pull do not require a successful 'guix' spec evaluation build
      --server=URL      Cuirass server URL (default: https://ci.guix.gnu.org)
  -v, --verbose         show per-evaluation status while scanning
  -h, --help            display this help and exit

Examples:
  guix repl -- guix-sync-commit.scm --max-pending=0
  guix repl -- guix-sync-commit.scm emacs git gcc-toolchain@14
")
  (exit 0))

(define (alist-set alist key val)
  (cons (cons key val)
        (alist-delete key alist eq?)))

(define (parse-args args)
  (let loop ((args args)
             (opts `((limit . ,%default-limit)
                     (system . #f)
                     (verbose? . #f)
                     (max-pending . 0)
                     (allow-failed? . #f)
                     (require-pull? . #t)
                     (server . ,%default-server)
                     (operands . ()))))
    (match args
      (() opts)
      ((or ("-h" . _) ("--help" . _))
       (show-help))
      (((or "-v" "--verbose") . rest)
       (loop rest (alist-set opts 'verbose? #t)))
      (("--allow-failed" . rest)
       (loop rest (alist-set opts 'allow-failed? #t)))
      (("--require-pull" . rest)
       (loop rest (alist-set opts 'require-pull? #t)))
      (("--no-require-pull" . rest)
       (loop rest (alist-set opts 'require-pull? #f)))
      (((? (lambda (s) (string-prefix? "--limit=" s)) s) . rest)
       (loop rest (alist-set opts 'limit (string->number (substring s 8)))))
      (("-n" n . rest)
       (loop rest (alist-set opts 'limit (string->number n))))
      (((? (lambda (s) (string-prefix? "--system=" s)) s) . rest)
       (loop rest (alist-set opts 'system (substring s 9))))
      (("-s" sys . rest)
       (loop rest (alist-set opts 'system sys)))
      (((? (lambda (s) (string-prefix? "--server=" s)) s) . rest)
       (loop rest (alist-set opts 'server (substring s 9))))
      (((? (lambda (s) (string-prefix? "--max-pending=" s)) s) . rest)
       (loop rest (alist-set opts 'max-pending (string->number (substring s 14)))))
      ((opt . rest)
       (if (string-prefix? "-" opt)
           (begin
             (format (current-error-port) "Error: unknown option ~a~%" opt)
             (exit 1))
           (loop rest (alist-set opts 'operands
                                 (append (assoc-ref opts 'operands) (list opt)))))))))

;; Parse a package specification like "gcc-toolchain@14:debug" into ("gcc-toolchain" "14").
;; Any ":output" suffix is stripped first because Cuirass tracks builds at the package level.
(define (spec->name+ver spec)
  (let* ((no-output (car (string-split spec #\:)))
         (at (string-index no-output #\@)))
    (if at
        (values (substring no-output 0 at) (substring no-output (+ 1 at)))
        (values no-output #f))))

;; Extract the "<package>-<version>" nixname from a store derivation path
;; (e.g. "/gnu/store/<hash>-gcc-toolchain-14.2.0.drv" -> "gcc-toolchain-14.2.0").
(define (extract-nixname drv)
  (if (not drv)
      ""
      (let* ((base (basename drv))
             (dash (string-index base #\-)))
        (if dash
            (let ((name (substring base (+ 1 dash))))
              (if (string-suffix? ".drv" name)
                  (substring name 0 (- (string-length name) 4))
                  name))
            base))))

;; Strip the "<name>-" prefix from a nixname to yield the version string.
(define (nixname->version name nixname)
  (if (string-prefix? (string-append name "-") nixname)
      (substring nixname (+ (string-length name) 1))
      nixname))

;; Cache build derivation -> nixname to minimize queries to /build/<id>.
(define %build-cache (make-hash-table))

(define (get-build-nixname server build-id)
  (or (hash-ref %build-cache build-id)
      (let* ((b (catch #t
                  (lambda () (build server build-id))
                  (lambda _ #f)))
             (drv (and b (build-derivation b)))
             (nix (extract-nixname drv)))
        (hash-set! %build-cache build-id nix)
        nix)))

;; Filter and resolve the jobs matching a package specification (e.g. "gcc-toolchain@14").
;; When multiple versions of a package exist in the same evaluation, /build/<id> is queried
;; lazily to extract the version from the derivation path and either filter by the requested
;; version prefix or pick the highest version according to Guix's version>?.
(define (resolve-jobs-for-spec server spec target-system jobs)
  (let-values (((name req-ver) (spec->name+ver spec)))
    (let* ((target-suffix (string-append "." target-system))
           (matched-jobs
            (filter (lambda (j)
                      (let ((jname (job-name j)))
                        (or (string=? jname (string-append name target-suffix))
                            (string=? jname (string-append "base." name target-suffix)))))
                    jobs)))
      (cond
       ((null? matched-jobs)
        '())
       ;; Fast path: if all matching jobs succeeded and no version filter was requested, it is ready.
       ((and (not req-ver)
             (every (lambda (j) (eq? (job-status j) 'succeeded)) matched-jobs))
        matched-jobs)
       ;; Only a single job exists for this package name.
       ((null? (cdr matched-jobs))
        matched-jobs)
       ;; Multiple jobs exist: resolve versions from /build/<id>.
       (else
        (let* ((jobs-with-ver
                (map (lambda (j)
                       (let* ((nix (get-build-nixname server (job-build-id j)))
                              (ver (nixname->version name nix)))
                         (cons ver j)))
                     matched-jobs)))
          (if req-ver
              (let ((filtered (filter (lambda (entry)
                                        (string-prefix? req-ver (car entry)))
                                      jobs-with-ver)))
                (if (null? filtered)
                    matched-jobs
                    (map cdr filtered)))
              ;; Pick the job with the latest version.
              (let* ((sorted (sort jobs-with-ver
                                   (lambda (a b)
                                     (catch #t
                                       (lambda () (version>? (car a) (car b)))
                                       (lambda _ #f))))))
                (list (cdr (car sorted)))))))))))

(define (chunk-list lst n)
  (let loop ((lst lst) (acc '()))
    (if (null? lst)
        (reverse acc)
        (let ((head (take lst (min n (length lst))))
              (tail (drop lst (min n (length lst)))))
          (loop tail (cons head acc))))))

(define-record-type <history-entry>
  (make-history-entry evaluation checkouts jobs)
  history-entry?
  (evaluation history-entry-evaluation)
  (checkouts history-entry-checkouts)
  (jobs history-entry-jobs))

;; Fetch /api/jobs/history in small batches of 20 job names at a time to avoid
;; triggering Cuirass 504 Gateway Time-out errors on large package lists, then
;; merge the per-evaluation job lists back together preserving evaluation order.
(define (fetch-chunked-history server job-names limit)
  (let* ((chunk-size 20)
         (chunks (chunk-list job-names chunk-size))
         (histories
          (filter-map
           (lambda (chk)
             (catch #t
               (lambda ()
                 (jobs-history server chk #:specification "master" #:limit limit))
               (lambda (key . err-args)
                 (format (current-error-port) "Warning: chunk failed (~a), skipping batch~%" key)
                 #f)))
           chunks)))
    (if (null? histories)
        '()
        (let ((merged-table (make-hash-table)))
          (for-each
           (lambda (h-list)
             (for-each
              (lambda (h)
                (let* ((eid (history-evaluation h))
                       (existing (hash-ref merged-table eid)))
                  (if existing
                      (hash-set! merged-table eid
                                 (make-history-entry eid
                                                     (history-entry-checkouts existing)
                                                     (append (history-entry-jobs existing)
                                                             (history-jobs h))))
                      (hash-set! merged-table eid
                                 (make-history-entry eid
                                                     (history-checkouts h)
                                                     (history-jobs h))))))
              h-list))
           histories)
          ;; Preserve ordering of evaluations from the first valid chunk.
          (map (lambda (h)
                 (let ((eid (history-evaluation h)))
                   (hash-ref merged-table eid)))
               (car histories))))))

(define (hist-evaluation h)
  (if (history-entry? h)
      (history-entry-evaluation h)
      (history-evaluation h)))

(define (hist-checkouts h)
  (if (history-entry? h)
      (history-entry-checkouts h)
      (history-checkouts h)))

(define (hist-jobs h)
  (if (history-entry? h)
      (history-entry-jobs h)
      (history-jobs h)))

;; Check whether the "guix" jobset evaluation for COMMIT has finished building
;; all jobs for TARGET-SYSTEM. Returns (values pull-eval-id pull-ready?).
(define (check-guix-pull-status server commit target-system)
  (let* ((pull-eval (catch #t
                      (lambda ()
                        (let ((pull-evals (evaluations-for-commit server commit 10)))
                          (find (lambda (e) (string=? (evaluation-spec e) "guix")) pull-evals)))
                      (lambda _ #f)))
         (pull-eval-id (and pull-eval (evaluation-id pull-eval)))
         (pull-ready? (and pull-eval-id
                           (catch #t
                             (lambda ()
                               (let ((jobs (evaluation-jobs server pull-eval-id)))
                                 (and (pair? jobs)
                                      (let ((sys-jobs (filter (lambda (j)
                                                                (string-suffix? (string-append "." target-system) (job-name j)))
                                                              jobs)))
                                        (and (pair? sys-jobs)
                                             (every (lambda (j) (eq? (job-status j) 'succeeded)) sys-jobs))))))
                             (lambda _ #f)))))
    (values pull-eval-id pull-ready?)))

;; Main execution routine.
(define (main args)
  (let* ((opts (parse-args (cdr args)))
         (limit (assoc-ref opts 'limit))
         (target-system (or (assoc-ref opts 'system) (%current-system)))
         (server (assoc-ref opts 'server))
         (verbose? (assoc-ref opts 'verbose?))
         (max-pending (assoc-ref opts 'max-pending))
         (allow-failed? (assoc-ref opts 'allow-failed?))
         (require-pull? (assoc-ref opts 'require-pull?))
         (all-specs (delete-duplicates (assoc-ref opts 'operands)))
         (package-mode? (pair? all-specs)))

    (format #t "Querying ~a (system: ~a, limit: ~a evaluations)...~%" server target-system limit)

    (if package-mode?
        ;; PACKAGE MODE
        (let* ((job-names
                (delete-duplicates
                 (append (map (lambda (s)
                                (let-values (((n _) (spec->name+ver s)))
                                  (string-append n "." target-system)))
                              all-specs)
                         (map (lambda (s)
                                (let-values (((n _) (spec->name+ver s)))
                                  (string-append "base." n "." target-system)))
                              all-specs))))
               (history (fetch-chunked-history server job-names limit)))

          (when (null? history)
            (format (current-error-port) "No history returned for specified packages.~%")
            (exit 1))

          ;; Identify specs that are not tracked in CI at all so local/custom packages do not block.
          (let* ((all-returned-jobs (append-map hist-jobs history))
                 (all-returned-names (delete-duplicates (map job-name all-returned-jobs)))
                 (untracked-specs
                  (filter (lambda (s)
                            (let-values (((n _) (spec->name+ver s)))
                              (not (or (member (string-append n "." target-system) all-returned-names)
                                       (member (string-append "base." n "." target-system) all-returned-names)))))
                          all-specs))
                 (tracked-specs
                  (filter (lambda (s) (not (member s untracked-specs))) all-specs)))

            (when (pair? untracked-specs)
              (format #t "Notice: ~a package(s) not built by CI (built locally or custom channel):~%  ~a~%"
                      (length untracked-specs) (string-join untracked-specs " ")))

            (when (null? tracked-specs)
              (format (current-error-port) "None of the specified packages are tracked on CI.~%")
              (exit 1))

            (format #t "Scanning evaluations for ~a tracked package(s)...~%" (length tracked-specs))

            (let loop ((entries history)
                       (idx 0))
              (if (null? entries)
                  (begin
                    (format (current-error-port) "No evaluation found in the last ~a evaluations where all packages are ready.~%" limit)
                    (exit 1))
                  (let* ((h (car entries))
                         (eval-id (hist-evaluation h))
                         (commit (match (hist-checkouts h)
                                   ((co . _) (checkout-commit co))
                                   (_ "unknown")))
                         (jobs (hist-jobs h))
                         ;; Evaluate readiness of each tracked spec.
                         (spec-results
                          (map (lambda (spec)
                                 (let ((matched (resolve-jobs-for-spec server spec target-system jobs)))
                                   (if (null? matched)
                                       (cons spec 'missing)
                                       (let* ((statuses (map job-status matched))
                                              (has-pending (any (lambda (s) (memq s %pending-statuses)) statuses))
                                              (has-failed (any (lambda (s) (memq s %failed-statuses)) statuses)))
                                         (cond
                                          (has-pending (cons spec 'pending))
                                          (has-failed (if allow-failed? (cons spec 'succeeded) (cons spec 'failed)))
                                          (else (cons spec 'succeeded)))))))
                               tracked-specs))
                         (all-succeeded? (every (lambda (pair) (eq? (cdr pair) 'succeeded)) spec-results)))

                    (when verbose?
                      (let ((pending-count (count (lambda (pair) (eq? (cdr pair) 'pending)) spec-results))
                            (failed-count (count (lambda (pair) (eq? (cdr pair) 'failed)) spec-results)))
                        (format #t "  [eval ~a] commit ~a -> ~a (pending: ~a, failed: ~a)~%"
                                eval-id (substring commit 0 (min 8 (string-length commit)))
                                (if all-succeeded? "READY" "WAITING")
                                pending-count failed-count)))

                    (if all-succeeded?
                        (let-values (((pull-eval-id pull-ready?)
                                      (check-guix-pull-status server commit target-system)))
                          (if (and require-pull? (not pull-ready?))
                              (loop (cdr entries) (+ idx 1))
                              (print-result server eval-id commit idx #f untracked-specs pull-eval-id pull-ready?)))
                        (loop (cdr entries) (+ idx 1))))))))

        ;; WHOLE-EVALUATION MODE
        (let* ((fetch-evals
                (lambda ()
                  (let try ((attempt 1))
                    (catch #t
                      (lambda ()
                        (latest-evaluations server limit #:spec "master"))
                      (lambda (key . err-args)
                        (if (< attempt 3)
                            (begin
                              (format (current-error-port) "Warning: could not fetch evaluations (~a), retrying (~a/3)...~%" key attempt)
                              (sleep 2)
                              (try (+ attempt 1)))
                            (begin
                              (format (current-error-port) "Error fetching evaluations from ~a: ~a~%" server key)
                              (exit 1))))))))
               (evals (fetch-evals)))

          (when (null? evals)
            (format (current-error-port) "No master evaluations found.~%")
            (exit 1))

          (format #t "Scanning master evaluations (max-pending: ~a)...~%" max-pending)

          (let loop ((ev-list evals)
                     (idx 0))
            (if (null? ev-list)
                (begin
                  (format (current-error-port) "No evaluation found with pending <= ~a in the last ~a evaluations.~%" max-pending limit)
                  (exit 1))
                (let* ((ev (car ev-list))
                       (eval-id (evaluation-id ev))
                       (commit (match (evaluation-checkouts ev)
                                 ((co . _) (checkout-commit co))
                                 (_ "unknown")))
                       (ev-date (evaluation-start-time ev)))

                  (format #t "Checking evaluation ~a (commit ~a)... "
                          eval-id (substring commit 0 (min 8 (string-length commit))))
                  (force-output)

                  (let ((jobs (let try ((attempt 1))
                                (catch #t
                                  (lambda () (evaluation-jobs server eval-id))
                                  (lambda (k . a)
                                    (if (< attempt 2)
                                        (begin
                                          (sleep 2)
                                          (try (+ attempt 1)))
                                        (begin
                                          (format (current-error-port) "error fetching jobs (~a)~%" k)
                                          #f)))))))
                    (if (not jobs)
                        (loop (cdr ev-list) (+ idx 1))
                        (let* ((sys-jobs (filter (lambda (j)
                                                    (string-suffix? (string-append "." target-system) (job-name j)))
                                                   jobs))
                               (pending-jobs (filter (lambda (j)
                                                       (memq (job-status j) %pending-statuses))
                                                     sys-jobs))
                               (pending-count (length pending-jobs)))

                          (format #t "~a pending~%" pending-count)

                          (when verbose?
                            (when (pair? pending-jobs)
                              (format #t "    Pending samples: ~a~%"
                                      (string-join (map job-name (take pending-jobs (min 5 pending-count))) ", "))))

                          (if (<= pending-count max-pending)
                              (let-values (((pull-eval-id pull-ready?)
                                            (check-guix-pull-status server commit target-system)))
                                (if (and require-pull? (not pull-ready?))
                                    (loop (cdr ev-list) (+ idx 1))
                                    (print-result server eval-id commit idx ev-date '() pull-eval-id pull-ready?)))
                              (loop (cdr ev-list) (+ idx 1)))))))))))))

(define (print-result server eval-id commit lag-idx ev-date untracked-specs pull-eval-id pull-ready?)
  (let ((date-str (if ev-date
                      (date->string ev-date "~Y-~m-~d ~H:~M UTC")
                      (catch #t
                        (lambda ()
                          (let ((ev (evaluation server eval-id)))
                            (date->string (evaluation-start-time ev) "~Y-~m-~d ~H:~M UTC")))
                        (lambda _ "unknown")))))
    (format #t "~%========================================================================~%")
    (format #t "Newest ready commit: ~a~%" commit)
    (format #t "  Date:              ~a~%" date-str)
    (format #t "  Evaluation:        ~a (spec: master, ~a)~%"
            eval-id
            (if (= lag-idx 0) "latest evaluation" (format #f "~a evaluations behind master" lag-idx)))
    (when pull-eval-id
      (format #t "  Guix Jobset Eval:  ~a (jobset: guix)~%" pull-eval-id))
    (when (pair? untracked-specs)
      (format #t "  Skipped local:     ~a~%" (string-join untracked-specs " ")))
    (if pull-ready?
        (format #t "  guix pull status:  Ready (prebuilt binary available for Guix itself)~%")
        (format #t "  guix pull status:  Building or pending in 'guix' jobset; expected local compile of Guix modules~%"))

    (format #t "~%To update your channels:~%")
    (format #t "  guix pull --commit=~a~%~%" commit)
    (format #t ";; In ~~/.config/guix/channels.scm:~%")
    (format #t "(list (channel~%")
    (format #t "       (inherit %default-guix-channel)~%")
    (format #t "       (url \"https://codeberg.org/guix/guix.git\")~%")
    (format #t "       (commit \"~a\")~%" commit)
    (format #t "       (introduction~%")
    (format #t "        (make-channel-introduction~%")
    (format #t "         \"9edb3f66fd807b096b48283debdcddccfea34bad\"~%")
    (format #t "         (openpgp-fingerprint~%")
    (format #t "          \"BBB0 2DDF 2CEA F6A8 0D1D  E643 A2A0 6DF2 A33A 54FA\")))))~%")
    (format #t "========================================================================~%")
    (exit 0)))

(main (command-line))
