// LawPay hosted fields loader. The card or bank details are typed into LawPay's own
// fields; this script only asks LawPay for a one-time token and puts it in the form. No
// card or bank number ever reaches the module.
(function () {
  var form = document.getElementById("pay-form");
  if (!form) { return; }
  var key = form.getAttribute("data-public-key");
  var src = form.getAttribute("data-hosted-fields");
  var out = document.getElementById("pay-error");
  var script = document.createElement("script");
  script.src = src;
  script.onload = function () {
    var fields = window.AffiniPay.HostedFields.initializeFields({
      publicKey: key,
      fields: [
        { selector: "#card-number", input: { type: "credit_card_number" } },
        { selector: "#card-exp", input: { type: "credit_card_expiration" } },
        { selector: "#card-cvv", input: { type: "credit_card_cvv" } }
      ]
    }, function () {});
    form.addEventListener("submit", function (ev) {
      if (form.elements.token.value) { return; }
      ev.preventDefault();
      fields.getPaymentToken({}).then(function (t) {
        form.elements.token.value = t.id;
        form.submit();
      }).catch(function () {
        out.textContent = "Check the card details and try again.";
      });
    });
  };
  document.head.appendChild(script);
})();
