// The location or category tree editor, embedded as a modal escape hatch for
// any location or category field rendered by a review screen
// (docs/specs/26-location-quick-create.md, docs/specs/27-category-quick-create.md).
//
// This is a second embedding of components locations.html and categories.html
// already use — js/tree.js itself, and the same create/rename/move calls
// against GET/POST/PATCH /api/storages/{storage_id}/locations or
// /categories (docs/specs/06-vision-shelf-ingestion.md,
// docs/specs/08-expiration-and-classification.md) — never a new tree UI and
// never a second same-storage validation path. The server is still the only
// thing that checks a node belongs to storageId; this module only ever calls
// it with the storageId its caller supplied, and never navigates the page.
//
// One module, parametrized by kind, instead of two nearly-identical ones
// (docs/specs/27-category-quick-create.md: "generalizes 26's modal instead
// of duplicating it"). The categories kind's shelf-life-rule detail beside
// each node is the exact renderer categories.html uses, from
// js/category-shelf-life.js — not a stripped-down copy of it.

import { TreeView } from "./tree.js";
import { get, post, patch, ApiError } from "./api.js";
import { el, text, clearChildren } from "./dom.js";
import { createShelfLifeDetail, resolveInheritance } from "./category-shelf-life.js";
import { t, apiErrorMessage } from "./i18n.js";

// The locations kind's DOM ids keep their pre-rename "location-modal-*"
// spelling on purpose: e2e/specs/ingestion.spec.js and
// e2e/specs/shopping-list.spec.js already hardcode
// "#location-modal-add-root-form" for the location-quick-create tests this
// module's predecessor shipped with, and this generalization changes the
// module's shape, not its locations behavior or its tests
// (docs/specs/27-category-quick-create.md). The categories kind, being new,
// gets its own "category-modal-*" ids below rather than inheriting these.
const KINDS = {
  locations: {
    endpoint: "locations",
    get title() { return t("locations.title"); },
    titleId: "location-modal-title",
    addRootFormId: "location-modal-add-root-form",
    get hint() { return t("locations.hint"); },
    get addRootLabel() { return t("locations.addRoot"); },
    get rootNameLabel() { return t("locations.addRootNameLabel"); },
    get rootNamePlaceholder() { return t("locations.addRootNamePlaceholder"); },
    get emptyMessage() { return t("treeModal.locations.empty"); },
  },
  categories: {
    endpoint: "categories",
    get title() { return t("categories.title"); },
    titleId: "category-modal-title",
    addRootFormId: "category-modal-add-root-form",
    get hint() { return t("categories.hint"); },
    get addRootLabel() { return t("categories.addRoot"); },
    get rootNameLabel() { return t("categories.addRootNameLabel"); },
    get rootNamePlaceholder() { return t("categories.addRootNamePlaceholder"); },
    get emptyMessage() { return t("treeModal.categories.empty"); },
  },
};

// True while a dialog opened by openTreeManager is on document.body, of
// either kind. location-options.js's openLocationField and
// category-options.js's openCategoryField each disable only their own
// trigger before calling openTreeManager, so nothing stops a location
// trigger's dialog and a category trigger's dialog from stacking at once
// (#144) — per-trigger disabling can't fix that, since neither caller knows
// about the other kind's trigger, so the guard lives here once instead.
let dialogOpen = false;

/**
 * openTreeManager renders js/tree.js inside a native <dialog>, scoped to
 * storageId and to one kind of tree. Resolves once the dialog is dismissed —
 * "Done", Esc, or a backdrop click, all equivalent — with the ids of any
 * nodes created during that session, in creation order.
 *
 * Same contract as docs/specs/26-location-quick-create.md's
 * openLocationManager(storageId) used to have, parametrized by which tree
 * js/tree.js renders inside the dialog.
 *
 * A second call of either kind, while a dialog from an earlier call is still
 * open, is refused rather than queued: it resolves immediately with no
 * created ids and never touches the DOM. Simpler than queuing, and nothing
 * in docs/specs/26 or docs/specs/27 asks for a second attempt to eventually
 * open once the first closes.
 *
 * `opened` is how a caller tells that refusal from a dialog that really ran
 * and happened to create nothing — both carry an empty createdIds, but only
 * the second is a reason to refresh anything. A caller that ignores it turns
 * a deliberate no-op into a network request, and a hiccup on that request
 * into an error message for a click the application chose not to act on
 * (#227).
 *
 * @param {string} storageId
 * @param {{kind: "locations"|"categories"}} options
 * @returns {Promise<{opened: boolean, createdIds: string[]}>}
 */
export function openTreeManager(storageId, { kind }) {
  if (dialogOpen) return Promise.resolve({ opened: false, createdIds: [] });
  dialogOpen = true;

  const config = KINDS[kind];
  const createdIds = [];
  // pendingMutations holds every runMutation() attempt still in flight (each
  // catches internally, so none ever rejects); requestClose waits until the
  // set is empty before finalizing, instead of resolving createdIds without
  // an id the create POST hasn't returned yet and racing the caller's own
  // refresh against that same in-flight write.
  //
  // A set rather than a single slot naming "the most recently started
  // mutation", because nothing orders POST responses by request order. With
  // a slot, an earlier-started mutation that settles *after* a later-started
  // one is lost: the later one finds the slot still naming itself, clears
  // it, and a requestClose waiting on it then sees an empty slot and
  // finalizes while the earlier one's create is still in flight — its id
  // never reaching createdIds and its late answer landing in a dialog
  // already removed (#221). Membership of a set is independent of settle
  // order, so the question "is anything still outstanding?" stops having a
  // wrong answer.
  //
  // An attempt stays in the set until its reload has redrawn the dialog too,
  // not merely until its own mutate() has answered (#280). Membership means
  // "this mutation is not finished with the dialog yet", and redrawing the
  // dialog is part of being finished with it: deleting on mutate()'s settle
  // left a window in which the set was already empty while the reload GET was
  // still outstanding, so a Done or Esc inside it finalized and that reload's
  // own view.render — or its showError — then landed on a dialog already
  // removed from the DOM.
  const pendingMutations = new Set();

  // unacknowledgedFailures holds one token per attempt whose error the user has
  // not yet been given the chance to read. A set of per-attempt tokens rather
  // than one shared boolean, for exactly the reason pendingMutations is a set:
  // a single flag names "the current mutation" in a function that can have
  // several in flight, and both ways that went wrong were real. A second
  // mutation starting reset the flag and cleared the banner, so a first
  // mutation's failure could disappear before anyone read it (#281); and
  // finalize consumed the flag, so of two dismissals asked for before the same
  // mutation failed, the first correctly kept the dialog open and the second
  // found the flag already cleared and closed it — in the same microtask batch,
  // before the error had ever been painted (#275).
  //
  // Nothing is ever removed from this set: a failure stops holding the dialog
  // open not by being consumed but by appearing in the snapshot requestClose
  // takes at the user's gesture (see requestClose), which is what tells a
  // failure the user has had in front of them from one that appeared while
  // their dismissal was already waiting.
  const unacknowledgedFailures = new Set();

  // Every failure gets its own banner inside this one live region, instead of
  // one box whose text the next failure overwrites (#347). Each banner names the
  // operation it belongs to — "Could not rename “Pantry”." — so two failures are
  // both readable and neither can be mistaken for the other's, which a single
  // shared box could not manage however its text was handled: the last text
  // painted was all there was, and it said nothing about whose it was.
  //
  // The region carries role="alert" and its children deliberately do not, so the
  // dialog has exactly one alert node however many failures are on screen, and
  // an appended banner is announced without a second live region appearing.
  //
  // Two things this deliberately does not do, which is the decision #347 asked
  // for about what an acknowledged-but-still-open failure looks like:
  //
  // - A banner never shows whether its failure has been acknowledged. The
  //   snapshot requestClose takes decides whether a *dismissal* may close the
  //   dialog; it says nothing about whether a message is still true, and every
  //   banner here names a mutation that did not happen, which stays true either
  //   way. Rendering it would also mean lifting that snapshot out of
  //   requestClose's local scope into shared mutable state — the exact shape
  //   #275 was.
  // - No banner carries a dismiss control. Removing the attempt's token with it
  //   would reintroduce a consume-once transition on the failure ledger, which
  //   is #275; leaving the token would hide a message the dialog still refuses
  //   to close over, which is worse for the user than the banner is. The
  //   dismissal that ends these is the dialog's own — closing it is what clears
  //   them.
  const errorList = el("div", { class: "stack", role: "alert", hidden: true });
  const statusBox = el("p", { class: "empty-state", role: "status", hidden: true });
  const hint = el("p", { class: "empty-state" }, [text(config.hint)]);
  const addRootButton = el("button", { type: "button", class: "btn" }, [text(config.addRootLabel)]);
  const treeContainer = el("div");
  const doneButton = el("button", { type: "button", class: "btn btn--primary" }, [text(t("treeModal.done"))]);

  const dialog = el("dialog", { class: "card stack", "aria-labelledby": config.titleId }, [
    el("h2", { id: config.titleId }, [text(config.title)]),
    errorList,
    statusBox,
    el("div", { class: "row row--between" }, [hint, addRootButton]),
    treeContainer,
    el("div", { class: "row" }, [doneButton]),
  ]);

  function basePath() {
    return `/api/storages/${storageId}/${config.endpoint}`;
  }

  // inherited is only ever populated for the categories kind (see reload
  // below); the locations kind's renderDetail stays unset here, as it did
  // on this module's predecessor (js/location-modal.js) before this spec.
  // locations.html's own page-level TreeView does pass one — the audit
  // staleness detail (docs/specs/13-stocktake-and-audit.md) — but that was
  // never part of the modal's own contract (docs/specs/26-location-quick-create.md)
  // and adding it here is out of this spec's scope.
  let inherited = new Map();

  // Each callback composes its own failure sentence here, at the gesture,
  // rather than leaving showError to invent one when the attempt eventually
  // fails: by then a concurrent mutation's reload may have redrawn the tree
  // under it, and the banner has to say what the user asked for, not what the
  // tree happens to look like when the answer comes back.
  const view = new TreeView(treeContainer, {
    onAddChild: (parentId, name) =>
      runMutation(() => createNode(parentId, name), t("treeModal.failed.add", { name })),
    onRename: (id, name) =>
      runMutation(
        () => patch(`${basePath()}/${id}`, { name }),
        nodeFailureMessage("treeModal.failed.rename", id, name),
      ),
    onMove: (id, newParentId) =>
      runMutation(
        () => patch(`${basePath()}/${id}`, { parent_id: newParentId }),
        nodeFailureMessage("treeModal.failed.move", id),
      ),
    renderDetail:
      kind === "categories"
        ? createShelfLifeDetail({ basePath, runMutation, showStatus, getInherited: () => inherited })
        : undefined,
  });

  /**
   * nodeFailureMessage renders one of the `treeModal.failed.*` sentences for a
   * mutation tree.js identified by id alone. onRename and onMove are handed an
   * id and no name, and a banner that cannot name the node it is about is the
   * whole defect #347 is about.
   *
   * The name comes from `view.nodes` — TreeView's own record of what it last
   * rendered — rather than a second map maintained here, so the banner names the
   * node as it was on screen when the user acted rather than as some later
   * reload found it.
   *
   * @param {string} key - a catalog key taking a single `{name}` placeholder.
   * @param {string} id
   * @param {string} [fallbackName] - used when `id` is not in the rendered tree;
   *   the new name, for a rename, which the user has just typed.
   * @returns {string}
   */
  function nodeFailureMessage(key, id, fallbackName) {
    const name = nodeName(id, view.nodes) ?? fallbackName;
    // Only reachable if tree.js called back for a node it never drew, which it
    // has no path to do — both callbacks hang off a rendered row. The generic
    // sentence is here so that if one ever appears it cannot read
    // `Could not move “undefined”.`
    return name === undefined ? t("treeModal.failed.change") : t(key, { name });
  }

  /**
   * nodeName finds one node's name in a rendered tree.
   *
   * @param {string} id
   * @param {import("./tree.js").TreeNode[]} nodes
   * @returns {string|undefined} undefined when no node in the tree has that id.
   */
  function nodeName(id, nodes) {
    for (const node of nodes) {
      if (node.id === id) return node.name;
      const found = node.children ? nodeName(id, node.children) : undefined;
      if (found !== undefined) return found;
    }
    return undefined;
  }

  function createNode(parentId, name) {
    return post(basePath(), { name, parent_id: parentId }).then((created) => {
      createdIds.push(created.id);
    });
  }

  /**
   * runMutation applies one change and redraws the dialog from the server's
   * answer. The server validates every mutation — same-storage membership, the
   * cycle rule on a move — so the dialog redraws from that answer rather than
   * predicting the new shape, exactly as locations.js and categories.js do.
   *
   * @param {() => Promise<void>} mutate
   * @param {string} failureMessage - a whole localized sentence naming this
   *   attempt, for the banner its failure would put up. Required: a banner that
   *   cannot name its attempt is the defect #347 is about, so there is no
   *   nameless default to fall back to.
   * @returns {Promise<void>} resolves when the attempt has finished with the
   *   dialog, whether it succeeded or failed — it never rejects.
   */
  async function runMutation(mutate, failureMessage) {
    // The status line belongs to whichever mutation is being started, so it is
    // always replaced. The error banners are not: they may be earlier mutations'
    // failures, and clearing them here is precisely how a second mutation used
    // to erase a first one's failure — banner and flag together — leaving Done
    // free to close the dialog with the user never having been told that the
    // first mutation did not happen (#281).
    //
    // Be clear about what that costs, because it is not a temporary state:
    // nothing ever removes a token from unacknowledgedFailures — it has no
    // .delete call, unlike pendingMutations below — so once any mutation has
    // failed, this guard stops clearing the banners for the rest of the dialog's
    // life. A failed create's banner therefore outlives every later
    // *successful* mutation and goes only when the dialog closes, which can
    // leave it standing beside a success.
    //
    // That is deliberate and it is forced: keeping a failure visible until the
    // user has been given the chance to read it and clearing it as soon as the
    // next mutation succeeds are the same decision with opposite answers, and
    // #281 is the bug report for choosing the second. It is also the second of
    // the two shapes #281 itself offers — "clear only messages belonging to the
    // mutation being started, or nothing at all while an unacknowledged failure
    // exists" — and since #347 the first shape is what happens as well, there
    // simply being no banner yet that belongs to the attempt being started.
    //
    // What a lingering banner no longer costs is attribution. It used to be
    // readable as the *current* mutation's failure, because one box held
    // whatever text was painted last and named no operation at all (#347); a
    // banner now names the attempt it belongs to, so an old one is old news
    // about a named mutation rather than a wrong statement about this one. Do
    // not add a read/acknowledge transition here to tidy that away — that is
    // the regression #281 was filed over.
    clearStatus();
    if (unacknowledgedFailures.size === 0) clearError();

    // One token per attempt, so both sets track attempts by identity rather
    // than by "the latest one". The tracked promise deliberately covers
    // mutate() *and* the reload that redraws the dialog from its answer, so
    // membership of pendingMutations lasts as long as this attempt still has
    // something to do to the dialog (#280).
    const token = {};
    const attempt = (async () => {
      try {
        await mutate();
      } catch (err) {
        // Latched the moment the banner is painted, which is what makes the
        // snapshot requestClose takes meaningful: a dismissal asked for after
        // this point has had the error in front of it, one already waiting when
        // it appeared has not.
        showError(err, failureMessage);
        unacknowledgedFailures.add(token);
      }
      // reload() reports instead of throwing, because it is also the initial
      // render's path and has nobody to throw to there. Its failure is latched
      // the same way a mutation's is: it paints a banner in the same dialog, so
      // a finalize that closed over it unseen would be the same bug in a
      // different place. It gets a banner of its own rather than amending the
      // mutation's, since the two say different things: the mutation may well
      // have happened and only the redraw have failed.
      if (!(await reload())) unacknowledgedFailures.add(token);
    })();

    pendingMutations.add(attempt);
    try {
      await attempt;
    } finally {
      // Leaves the set the moment the attempt settles — its reload included —
      // whoever else is still in it and whenever they started, which is the
      // whole point of tracking membership rather than "the latest one".
      pendingMutations.delete(attempt);
    }
  }

  /**
   * reload re-reads the tree and redraws the dialog from the server's answer.
   *
   * Its own failure sentence is fixed rather than a parameter: every caller's
   * GET fails in the same way and means the same thing — the list on screen is
   * not what the server has — whether the reload followed a mutation or is the
   * dialog's opening render.
   *
   * @returns {Promise<boolean>} true when the redraw happened, false when the
   *   GET failed and showError put a banner up in its place.
   */
  async function reload() {
    try {
      const body = await get(basePath());
      const empty = body.items.length === 0;
      hint.hidden = empty;
      if (empty) {
        clearChildren(treeContainer);
        treeContainer.append(el("p", { class: "empty-state" }, [text(config.emptyMessage)]));
        return true;
      }
      if (kind === "categories") inherited = resolveInheritance(body.items);
      view.render(body.items);
      return true;
    } catch (err) {
      showError(err, t("treeModal.failed.reload"));
      return false;
    }
  }

  /**
   * showError appends one banner for one failed attempt, naming what was being
   * attempted and then why it did not happen.
   *
   * The two are separate whole sentences from separate catalog keys, joined by a
   * space — never a sentence assembled out of fragments, which is what
   * docs/specs/19-localization.md forbids. The reason is the same string a single
   * shared box used to show on its own; what is new is the sentence in front of
   * it saying which attempt it belongs to (#347).
   *
   * @param {unknown} err - what the attempt threw, or the GET rejected with.
   * @param {string} failureMessage - a whole localized sentence naming the
   *   operation, composed by the caller before the attempt started.
   */
  function showError(err, failureMessage) {
    const reason = err instanceof ApiError ? apiErrorMessage(err) : t("treeModal.networkError");
    errorList.append(
      el("div", { class: "alert" }, [el("strong", {}, [text(failureMessage)]), text(` ${reason}`)]),
    );
    errorList.hidden = false;
  }

  function showStatus(message) {
    statusBox.textContent = message;
    statusBox.hidden = false;
  }

  // Clears every banner rather than one of them. The guard in runMutation is its
  // only caller and runs only while nothing at all is latched, so the single
  // banner this can ever discard is the one the opening reload() puts up — that
  // call site deliberately latches no token (see its comment below), which is
  // exactly what leaves the set empty with a real failure on screen. Discarding
  // that one is the right trade: the mutation now starting issues its own
  // reload, so the list is about to be redrawn or to fail again and say so.
  function clearError() {
    clearChildren(errorList);
    errorList.hidden = true;
  }

  function clearStatus() {
    statusBox.textContent = "";
    statusBox.hidden = true;
  }

  // showAddRootForm is the one case the tree component cannot cover: its
  // inline add-child editor opens underneath an existing node, and a first
  // top-level node has none to open under. Same inline pattern locations.js
  // and categories.js use, for the same reason (no prompt(), styleable,
  // reachable by assistive tech).
  addRootButton.addEventListener("click", () => {
    if (dialog.querySelector(`#${config.addRootFormId}`)) return;

    const input = el("input", {
      type: "text",
      "aria-label": config.rootNameLabel,
      placeholder: config.rootNamePlaceholder,
      required: true,
    });

    const form = el(
      "form",
      {
        id: config.addRootFormId,
        class: "row",
        onsubmit: (event) => {
          event.preventDefault();
          const name = input.value.trim();
          if (!name) return;
          form.remove();
          runMutation(() => createNode(null, name), t("treeModal.failed.add", { name }));
        },
      },
      [
        input,
        el("button", { type: "submit", class: "btn" }, [text(t("tree.add"))]),
        el("button", { type: "button", class: "btn btn--ghost", onclick: () => form.remove() }, [text(t("common.cancel"))]),
      ],
    );

    treeContainer.before(form);
    input.focus();
  });

  // requestClose is every dismissal's shared path — Done, a genuine backdrop
  // click, and Esc all route through it, so all three wait the same way for
  // a mutation that's still in flight instead of resolving createdIds without
  // it. If that mutation ended in an error, the dialog stays open on this
  // request so the user actually sees showError's message rather than it
  // landing in a dialog already removed; dismissing again — the error now in
  // front of them — closes it.
  //
  // INVARIANT: finalize only with pendingMutations empty, and re-check it
  // after every wait instead of finalizing once the batch awaited here
  // settles. Nothing stops a further mutation from starting while this
  // function is already waiting (the add-root form reopens the moment it's
  // submitted, before its own POST returns); such a mutation joins the set
  // and has to be waited for too. Recursing until the set is genuinely empty
  // is what makes both who-started-last and who-settles-last irrelevant —
  // finalizing on the first settled batch would resolve createdIds without
  // whatever a newer mutation is still in the middle of creating.
  //
  // The gesture is also where the set of failures the user has already been
  // shown is captured, once, and then carried unchanged through every wait
  // below. That snapshot is what separates "a failure that was on screen when
  // the user asked to close" from "a failure that appeared while this dismissal
  // was waiting", and it does so without consuming anything — which is exactly
  // what two dismissals racing one failing mutation need, since both of them
  // reach finalize in the same microtask batch and a consume-once flag can only
  // stop one of them (#275).
  function requestClose() {
    waitThenFinalize(new Set(unacknowledgedFailures));
  }

  function waitThenFinalize(seenFailures) {
    if (pendingMutations.size === 0) {
      finalize(seenFailures);
      return;
    }
    Promise.all([...pendingMutations]).then(() => waitThenFinalize(seenFailures));
  }

  // A failed mutation keeps the dialog open on every dismissal that was asked
  // for before its error was painted, so the user actually sees showError's
  // message instead of it landing in a dialog already removed. The next
  // dismissal — made with the banner in front of the user — carries that
  // failure in its own snapshot and closes normally, so a failure can never
  // trap the dialog open either.
  //
  // With two failures latched, the loop below is what makes both of them count:
  // a dismissal may close only when EVERY live token is in its own snapshot,
  // never when merely one of them is. Each failure has its own banner since
  // #347, so both are legible while that is being decided — which the single
  // shared error box this used to paint into could not offer, and which this
  // comment used to say was simply the price.
  //
  // Nothing is removed from unacknowledgedFailures, so any snapshot of it is a
  // subset of it, and "some live token is missing from the snapshot" and "the
  // two differ in size" are the same question today. The loop is written per
  // token because per-token identity is the property this needs; a size
  // comparison would stop being equivalent the moment anything ever deleted
  // from the set, and a check of one token — the newest, say — the moment
  // anything made a snapshot other than at the gesture.
  function finalize(seenFailures) {
    for (const failure of unacknowledgedFailures) {
      if (!seenFailures.has(failure)) return;
    }
    dialog.close();
  }

  return new Promise((resolve) => {
    doneButton.addEventListener("click", requestClose);

    // A click that lands on the dialog element itself, rather than on any of
    // its children, is USUALLY a click on the backdrop — but .card puts its
    // padding on the <dialog> element (css/components.css), so a click on
    // that padding also has event.target === dialog without being outside
    // the dialog's own box. Comparing the click's coordinates against the
    // dialog's rendered box is what actually tells backdrop and padding
    // apart.
    dialog.addEventListener("click", (event) => {
      if (event.target !== dialog) return;
      const box = dialog.getBoundingClientRect();
      const insideBox =
        event.clientX >= box.left &&
        event.clientX <= box.right &&
        event.clientY >= box.top &&
        event.clientY <= box.bottom;
      if (!insideBox) requestClose();
    });

    // Esc fires the dialog's own "cancel" then "close" natively; preventing
    // the default on "cancel" routes it through requestClose too, so Esc
    // waits for an in-flight mutation exactly like Done and the backdrop do.
    dialog.addEventListener("cancel", (event) => {
      event.preventDefault();
      requestClose();
    });

    dialog.addEventListener("close", () => {
      dialog.remove();
      dialogOpen = false;
      resolve({ opened: true, createdIds: [...createdIds] });
    });

    document.body.append(dialog);
    dialog.showModal();
    // The initial render, deliberately not tracked in pendingMutations and its
    // answer deliberately not latched. Unlike a mutation's reload, a dismissal
    // is allowed to win the race against this one: Done, the backdrop and Esc
    // are all wired above, *before* showModal(), so a dismissal genuinely can
    // arrive while this GET is still in flight — requestClose then finds the set
    // empty, finalize closes the dialog, and this reload's view.render or
    // showError afterwards runs against a treeContainer and errorList already
    // detached from the document.
    //
    // That is the same shape as #280 and it is still the behaviour wanted here,
    // which is why #280's own body excludes this call site: "reload() is also
    // called from other places that are not mutations (the initial render), so
    // the fix belongs in runMutation's tracking rather than inside reload()
    // itself." Nothing is at stake in this window, whatever else has happened in
    // the dialog by then: this reload's only casualty is a render, or a banner,
    // applied to nodes already detached from the document. It cannot cost a
    // created id — createdIds is resolved from its own array on the close event
    // and a redraw never feeds it — so a tracked mutation that started, finished
    // and emptied pendingMutations while this opening GET was still outstanding
    // loses nothing either, even though createdIds is not empty in that case.
    // Tracking this call, meanwhile, would make Esc do nothing at all until a
    // slow opening GET came back, which is a real cost for no gain. A banner
    // painted into a dialog the user has already dismissed is likewise not
    // something finalize should hold anything open for.
    reload();
  });
}
